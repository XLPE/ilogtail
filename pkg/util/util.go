// Copyright 2021 iLogtail Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package util

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"io/ioutil"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
	"unicode"
	"unsafe"

	"github.com/alibaba/ilogtail/pkg/protocol"
)

const alphanum string = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"

const (
	ShardHashTagKey = "__shardhash__"
	PackIDTagKey    = "__pack_id__"
)

var (
	ErrCommandTimeout = errors.New("command time out")
	ErrNotImplemented = errors.New("not implemented yet")
	ErrInvalidEnvType = errors.New("invalid env type")
)

// ReadLines reads contents from a file and splits them by new lines.
// A convenience wrapper to ReadLinesOffsetN(filename, 0, -1).
func ReadLines(filename string) ([]string, error) {
	return ReadLinesOffsetN(filename, 0, -1)
}

// ReadFirstBlock read first \S+ from head of line
func ReadFirstBlock(line string) string {
	for i, c := range line {
		// 32 -> [SPACE] 33 -> ! 126 -> ~ 127 -> [DEL]
		if c < 33 || c > 126 {
			return line[0:i]
		}
	}
	return line
}

// ReadLinesOffsetN reads contents from file and splits them by new line.
// The offset tells at which line number to start.
// The count determines the number of lines to read (starting from offset):
// n >= 0: at most n lines
// n < 0: whole file
func ReadLinesOffsetN(filename string, offset uint, n int) ([]string, error) {
	f, err := os.Open(filepath.Clean(filename))
	if err != nil {
		return []string{""}, err
	}
	defer func(f *os.File) {
		_ = f.Close()
	}(f)

	var ret []string

	r := bufio.NewReader(f)
	for i := 0; i < n+int(offset) || n < 0; i++ {
		line, err := r.ReadString('\n')
		if err != nil {
			break
		}
		if i < int(offset) {
			continue
		}
		ret = append(ret, strings.Trim(line, "\n"))
	}

	return ret, nil
}

// RandomString returns a random string of alpha-numeric characters
func RandomString(n int) string {
	var slice = make([]byte, n)
	_, _ = rand.Read(slice)
	for i, b := range slice {
		slice[i] = alphanum[b%byte(len(alphanum))]
	}
	return string(slice)
}

// GetTLSConfig gets a tls.Config object from the given certs, key, and CA files.
// you must give the full path to the files.
// If all files are blank and InsecureSkipVerify=false, returns a nil pointer.
func GetTLSConfig(sslCert, sslKey, sslCA string, insecureSkipVerify bool) (*tls.Config, error) {
	if sslCert == "" && sslKey == "" && sslCA == "" && !insecureSkipVerify {
		return nil, nil
	}

	t := &tls.Config{InsecureSkipVerify: insecureSkipVerify} //nolint:gosec

	if sslCA != "" {
		caCert, err := ioutil.ReadFile(filepath.Clean(sslCA))
		if err != nil {
			return nil, fmt.Errorf("Could not load TLS CA: %v", err)
		}

		caCertPool := x509.NewCertPool()
		caCertPool.AppendCertsFromPEM(caCert)
		t.RootCAs = caCertPool
	}

	if sslCert != "" && sslKey != "" {
		cert, err := tls.LoadX509KeyPair(sslCert, sslKey)
		if err != nil {
			return nil, fmt.Errorf("could not load TLS client key/certificate from %s:%s: %s", sslKey, sslCert, err)
		}

		t.Certificates = []tls.Certificate{cert}
		t.BuildNameToCertificate()
	}

	// will be nil by default if nothing is provided
	return t, nil
}

// SnakeCase converts the given string to snake case following the Golang format:
// acronyms are converted to lower-case and preceded by an underscore.
func SnakeCase(in string) string {
	runes := []rune(in)
	length := len(runes)

	var out []rune
	for i := 0; i < length; i++ {
		if i > 0 && unicode.IsUpper(runes[i]) && ((i+1 < length && unicode.IsLower(runes[i+1])) || unicode.IsLower(runes[i-1])) {
			out = append(out, '_')
		}
		out = append(out, unicode.ToLower(runes[i]))
	}

	return string(out)
}

// CombinedOutputTimeout runs the given command with the given timeout and
// returns the combined output of stdout and stderr.
// If the command times out, it attempts to kill the process.
func CombinedOutputTimeout(c *exec.Cmd, timeout time.Duration) ([]byte, error) {
	var b bytes.Buffer
	c.Stdout = &b
	c.Stderr = &b
	if err := c.Start(); err != nil {
		return nil, err
	}
	err := WaitTimeout(c, timeout)
	return b.Bytes(), err
}

// RunTimeout runs the given command with the given timeout.
// If the command times out, it attempts to kill the process.
func RunTimeout(c *exec.Cmd, timeout time.Duration) error {
	if err := c.Start(); err != nil {
		return err
	}
	return WaitTimeout(c, timeout)
}

// WaitTimeout waits for the given command to finish with a timeout.
// It assumes the command has already been started.
// If the command times out, it attempts to kill the process.
func WaitTimeout(c *exec.Cmd, timeout time.Duration) error {
	timer := time.NewTimer(timeout)
	done := make(chan error)
	go func() { done <- c.Wait() }()
	select {
	case err := <-done:
		timer.Stop()
		return err
	case <-timer.C:
		if err := c.Process.Kill(); err != nil {
			log.Printf("E! FATAL error killing process: %s", err)
			return err
		}
		// wait for the command to return after killing it
		<-done
		return ErrCommandTimeout
	}
}

// return true if shutdown is signaled
func RandomSleep(base time.Duration, precisionLose float64, shutdown <-chan struct{}) bool {
	// TODO: Last implementation costs too much CPU, find a better way to implement it.
	return Sleep(base, shutdown)
}

// Sleep returns true if shutdown is signaled.
func Sleep(interval time.Duration, shutdown <-chan struct{}) bool {
	select {
	case <-time.After(interval):
		return false
	case <-shutdown:
		return true
	}
}

func CutString(val string, maxLen int) string {
	if len(val) < maxLen {
		return val
	}
	return val[0:maxLen]
}

func GetCurrentBinaryPath() string {
	ex, err := os.Executable()
	if err != nil {
		return "./"
	}
	exPath := filepath.Dir(ex)
	return exPath + "/"
}

func PathExists(path string) (bool, error) {
	_, err := os.Stat(path)
	if err == nil {
		return true, nil
	}
	if os.IsNotExist(err) {
		return false, nil
	}
	return false, err
}

func SplitPath(path string) (dir string, filename string) {
	lastIndex := strings.LastIndexByte(path, '/')
	lastIndex2 := strings.LastIndexByte(path, '\\')
	if lastIndex < 0 && lastIndex2 < 0 {
		return "", ""
	}
	index := 0
	if lastIndex > lastIndex2 {
		index = lastIndex
	} else {
		index = lastIndex2
	}
	return path[0:index], path[index+1:]
}

func InitFromEnvBool(key string, value *bool, defaultValue bool) error {
	if envValue := os.Getenv(key); len(envValue) > 0 {
		lowErVal := strings.ToLower(envValue)
		if strings.HasPrefix(lowErVal, "y") || strings.HasPrefix(lowErVal, "t") || strings.HasPrefix(lowErVal, "on") || strings.HasPrefix(lowErVal, "ok") {
			*value = true
		} else {
			*value = false
		}
		return nil
	}
	*value = defaultValue
	return nil
}

func InitFromEnvInt(key string, value *int, defaultValue int) error {
	if envValue := os.Getenv(key); len(envValue) > 0 {
		if val, err := strconv.Atoi(envValue); err == nil {
			*value = val
			return nil
		}
		*value = defaultValue
		return ErrInvalidEnvType
	}
	*value = defaultValue
	return nil
}

func InitFromEnvString(key string, value *string, defaultValue string) error {
	if envValue := os.Getenv(key); len(envValue) > 0 {
		*value = envValue
		return nil
	}
	*value = defaultValue
	return nil
}

// GuessRegionByEndpoint guess region
// cn-hangzhou.log.aliyuncs.com cn-hangzhou-intranet.log.aliyuncs.com cn-hangzhou-vpc.log.aliyuncs.com cn-hangzhou-share.log.aliyuncs.com
func GuessRegionByEndpoint(endPoint, defaultRegion string) string {

	if strings.HasPrefix(endPoint, "http://") {
		endPoint = endPoint[len("http://"):]
	} else {
		endPoint = strings.TrimPrefix(endPoint, "https://")
	}
	if dotIndex := strings.IndexByte(endPoint, '.'); dotIndex > 0 {
		region := endPoint[0:dotIndex]
		if strings.HasSuffix(region, "-intranet") || strings.HasSuffix(region, "-vpc") || strings.HasSuffix(region, "-share") {
			region = region[0:strings.LastIndexByte(region, '-')]
		}
		return region
	}
	return defaultRegion
}

func DeepCopy(src *map[string]interface{}) *map[string]interface{} {
	if src == nil {
		return nil
	}
	var buf []byte
	var err error
	if buf, err = json.Marshal(src); err != nil {
		return nil
	}
	dst := new(map[string]interface{})
	if err := json.Unmarshal(buf, dst); err != nil {
		return nil
	}
	return dst
}

func InterfaceToString(val interface{}) (string, bool) {
	if val == nil {
		return "", false
	}
	strVal, ok := val.(string)
	return strVal, ok
}

func InterfaceToJSONString(val interface{}) (string, error) {
	b, err := json.Marshal(val)
	return string(b), err
}

func NewPackIDPrefix(text string) string {
	h := fnv.New64a()
	_, _ = h.Write([]byte(text + GetIPAddress() + time.Now().String()))
	return fmt.Sprintf("%X-", h.Sum64())
}

func NewLogTagForPackID(prefix string, seqNum *int64) *protocol.LogTag {
	tag := &protocol.LogTag{
		Key:   PackIDTagKey,
		Value: fmt.Sprintf("%s%X", prefix, atomic.LoadInt64(seqNum)),
	}
	atomic.AddInt64(seqNum, 1)
	return tag
}

// ParseVariableValue parse specific key with logic:
//  1. if key start with '$', the get from env
//  2. if key == __ip__, return local ip address
//  3. if key == __host__, return hostName
//     others return key
func ParseVariableValue(key string) string {
	if len(key) == 0 {
		return key
	}
	if key[0] == '$' {
		return os.Getenv(key[1:])
	}
	if key == "__ip__" {
		return ipAddress
	}
	if key == "__host__" {
		return hostName
	}
	return key
}

// Label for metric label
type Label struct {
	Name  string
	Value string
}

// Labels for metric labels
type Labels []Label

func (l Labels) Len() int {
	return len(l)
}

func (l Labels) Swap(i int, j int) {
	l[i], l[j] = l[j], l[i]
}

func (l Labels) Less(i int, j int) bool {
	return l[i].Name < l[j].Name
}

// DefBucket ...
type DefBucket struct {
	Le    float64
	Count int64
}

// HistogramData ...
type HistogramData struct {
	Buckets []DefBucket
	Count   int64
	Sum     float64
}

// ToMetricLogs ..
func (hd *HistogramData) ToMetricLogs(name string, timeMs int64, labels Labels) []*protocol.Log {
	logs := make([]*protocol.Log, 0, len(hd.Buckets)+2)
	sort.Sort(labels)
	for _, v := range hd.Buckets {
		newLabels := make(Labels, len(labels), len(labels)+1)
		copy(newLabels, labels)
		newLabels = append(newLabels, Label{Name: "le", Value: strconv.FormatFloat(v.Le, 'g', -1, 64)})
		sort.Sort(newLabels)
		logs = append(logs, NewMetricLog(name+"_bucket", timeMs, strconv.FormatInt(v.Count, 10), newLabels))
	}
	logs = append(logs, NewMetricLog(name+"_count", timeMs, strconv.FormatInt(hd.Count, 10), labels))
	logs = append(logs, NewMetricLog(name+"_sum", timeMs, strconv.FormatFloat(hd.Sum, 'g', -1, 64), labels))
	return logs
}

// NewMetricLog caller must sort labels
func NewMetricLog(name string, timeMs int64, value string, labels []Label) *protocol.Log {
	strTime := strconv.FormatInt(timeMs, 10)
	metric := &protocol.Log{Time: uint32(timeMs / 1000)}
	metric.Contents = []*protocol.Log_Content{}
	metric.Contents = append(metric.Contents, &protocol.Log_Content{Key: "__name__", Value: name})
	metric.Contents = append(metric.Contents, &protocol.Log_Content{Key: "__time_nano__", Value: strTime})

	builder := strings.Builder{}
	for index, l := range labels {
		if index != 0 {
			builder.WriteString("|")
		}
		builder.WriteString(l.Name)
		builder.WriteString("#$#")
		builder.WriteString(l.Value)

	}
	metric.Contents = append(metric.Contents, &protocol.Log_Content{Key: "__labels__", Value: builder.String()})

	metric.Contents = append(metric.Contents, &protocol.Log_Content{Key: "__value__", Value: value})
	return metric
}

func MinInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// StringDeepCopy returns a deep copy or src.
// Because we can not make sure the life cycle of string passed from C++,
// so we have to make a deep copy of them so that they are always valid in Go.
func StringDeepCopy(src string) string {
	return string([]byte(src))
}

// StringPointer returns the pointer of the given string.
// nolint:gosec
func StringPointer(s string) unsafe.Pointer {
	p := (*reflect.StringHeader)(unsafe.Pointer(&s))
	return unsafe.Pointer(p.Data)
}
