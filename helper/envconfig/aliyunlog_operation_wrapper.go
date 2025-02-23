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

package envconfig

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	k8s_event "github.com/alibaba/ilogtail/helper/eventrecorder"
	"github.com/alibaba/ilogtail/pkg/flags"
	"github.com/alibaba/ilogtail/pkg/logger"
	"github.com/alibaba/ilogtail/pkg/util"

	aliyunlog "github.com/aliyun/aliyun-log-go-sdk"
)

// nolint:unused
type configCheckPoint struct {
	ProjectName string
	ConfigName  string
}

type operationWrapper struct {
	logClient        aliyunlog.ClientInterface
	lock             sync.RWMutex
	project          string
	logstoreCacheMap map[string]time.Time
	configCacheMap   map[string]time.Time
}

func createDefaultK8SIndex() *aliyunlog.Index {
	normalIndexKey := aliyunlog.IndexKey{
		Token:         []string{" ", "\n", "\t", "\r", ",", ";", "[", "]", "{", "}", "(", ")", "&", "^", "*", "#", "@", "~", "=", "<", ">", "/", "\\", "?", ":", "'", "\""},
		CaseSensitive: false,
		Type:          "text",
		DocValue:      true,
	}
	return &aliyunlog.Index{
		Line: &aliyunlog.IndexLine{
			Token:         []string{" ", "\n", "\t", "\r", ",", ";", "[", "]", "{", "}", "(", ")", "&", "^", "*", "#", "@", "~", "=", "<", ">", "/", "\\", "?", ":", "'", "\""},
			CaseSensitive: false,
		},
		Keys: map[string]aliyunlog.IndexKey{
			"__tag__:__hostname__":     normalIndexKey,
			"__tag__:__path__":         normalIndexKey,
			"__tag__:_container_ip_":   normalIndexKey,
			"__tag__:_container_name_": normalIndexKey,
			"__tag__:_image_name_":     normalIndexKey,
			"__tag__:_namespace_":      normalIndexKey,
			"__tag__:_pod_name_":       normalIndexKey,
			"__tag__:_pod_uid_":        normalIndexKey,
			"_container_ip_":           normalIndexKey,
			"_container_name_":         normalIndexKey,
			"_image_name_":             normalIndexKey,
			"_namespace_":              normalIndexKey,
			"_pod_name_":               normalIndexKey,
			"_pod_uid_":                normalIndexKey,
			"_source_":                 normalIndexKey,
		},
	}
}

func addNecessaryInputConfigField(inputConfigDetail map[string]interface{}) map[string]interface{} {
	inputConfigDetailCopy := util.DeepCopy(&inputConfigDetail)
	aliyunlog.AddNecessaryInputConfigField(*inputConfigDetailCopy)
	logger.Debug(context.Background(), "before", inputConfigDetail, "after", *inputConfigDetailCopy)
	return *inputConfigDetailCopy
}

func createAliyunLogOperationWrapper(endpoint, project, accessKeyID, accessKeySecret, stsToken string, shutdown <-chan struct{}) (*operationWrapper, error) {
	var clientInterface aliyunlog.ClientInterface
	var err error
	if *flags.AliCloudECSFlag {
		// use UpdateTokenFunction to update token
		clientInterface, err = aliyunlog.CreateTokenAutoUpdateClient(endpoint, UpdateTokenFunction, shutdown)
		if err != nil {
			return nil, err
		}
	} else {
		clientInterface = aliyunlog.CreateNormalInterface(endpoint, accessKeyID, accessKeySecret, stsToken)
	}
	wrapper := &operationWrapper{
		logClient: clientInterface,
		project:   project,
	}
	logger.Info(context.Background(), "init aliyun log operation wrapper", "begin")
	// retry when make project fail
	for i := 0; i < 1; i++ {
		err = wrapper.makesureProjectExist(nil, project)
		if err == nil {
			break
		}
		logger.Warning(context.Background(), "CREATE_PROJECT_ALARM", "create project error, project", project, "error", err)
		time.Sleep(time.Second * time.Duration(30))
	}

	// retry when make project fail
	for i := 0; i < 3; i++ {
		err = wrapper.makesureMachineGroupExist(project, *flags.DefaultLogMachineGroup)
		if err == nil {
			break
		}
		logger.Warning(context.Background(), "CREATE_MACHINEGROUP_ALARM", "create machine group error, project", project, "error", err)
		time.Sleep(time.Second * time.Duration(30))
	}
	if err != nil {
		return nil, err
	}
	wrapper.logstoreCacheMap = make(map[string]time.Time)
	wrapper.configCacheMap = make(map[string]time.Time)
	logger.Info(context.Background(), "init aliyun log operation wrapper", "done")
	return wrapper, nil
}

func (o *operationWrapper) logstoreCacheExists(project, logstore string) bool {
	o.lock.RLock()
	defer o.lock.RUnlock()
	if lastUpdateTime, ok := o.logstoreCacheMap[project+"@@"+logstore]; ok {
		if time.Since(lastUpdateTime) < time.Second*time.Duration(*flags.LogResourceCacheExpireSec) {
			return true
		}
	}
	return false
}

func (o *operationWrapper) addLogstoreCache(project, logstore string) {
	o.lock.Lock()
	defer o.lock.Unlock()
	o.logstoreCacheMap[project+"@@"+logstore] = time.Now()
}

// nolint:unused
func (o *operationWrapper) configCacheExists(project, config string) bool {
	o.lock.RLock()
	defer o.lock.RUnlock()
	if lastUpdateTime, ok := o.configCacheMap[project+"@@"+config]; ok {
		if time.Since(lastUpdateTime) < time.Second*time.Duration(*flags.LogResourceCacheExpireSec) {
			return true
		}
	}
	return false
}

func (o *operationWrapper) addConfigCache(project, config string) {
	o.lock.Lock()
	defer o.lock.Unlock()
	o.configCacheMap[project+"@@"+config] = time.Now()
}

// nolint:unused
func (o *operationWrapper) removeConfigCache(project, config string) {
	o.lock.Lock()
	defer o.lock.Unlock()
	delete(o.configCacheMap, project+"@@"+config)
}

// nolint:unused
func (o *operationWrapper) retryCreateIndex(project, logstore string) {
	time.Sleep(time.Second * 10)
	index := createDefaultK8SIndex()
	// create index, create index do not return error
	for i := 0; i < 10; i++ {
		err := o.logClient.CreateIndex(project, logstore, *index)
		if err != nil {
			// if IndexAlreadyExist, just return
			if clientError, ok := err.(*aliyunlog.Error); ok && clientError.Code == "IndexAlreadyExist" {
				return
			}
			time.Sleep(time.Second * 10)
		} else {
			break
		}
	}
}

func (o *operationWrapper) createProductLogstore(config *AliyunLogConfigSpec, project, logstore, product, lang string) error {
	logger.Info(context.Background(), "begin to create product logstore, project", project, "logstore", logstore, "product", product, "lang", lang)
	err := CreateProductLogstore(*flags.DefaultRegion, project, logstore, product, lang)

	annotations := GetAnnotationByObject(config, project, logstore, product, config.LogtailConfig.ConfigName, false)

	if err != nil {
		if k8s_event.GetEventRecorder() != nil {
			customErr := CustomErrorFromPopError(err)
			k8s_event.GetEventRecorder().SendErrorEventWithAnnotation(k8s_event.GetEventRecorder().GetObject(), GetAnnotationByError(annotations, customErr), k8s_event.CreateProductLogStore, "", fmt.Sprintf("create product log failed, error: %s", err.Error()))
		}
		logger.Warning(context.Background(), "CREATE_PRODUCT_ALARM", "create product error, error", err)
		return err
	} else if k8s_event.GetEventRecorder() != nil {
		k8s_event.GetEventRecorder().SendNormalEventWithAnnotation(k8s_event.GetEventRecorder().GetObject(), annotations, k8s_event.CreateProductLogStore, "create product log success")
	}

	o.addLogstoreCache(project, logstore)
	return nil
}

func (o *operationWrapper) makesureLogstoreExist(config *AliyunLogConfigSpec, project, logstore string, shardCount, lifeCycle int, product, lang string) error {
	if o.logstoreCacheExists(project, logstore) {
		return nil
	}

	if len(product) > 0 {
		if len(lang) == 0 {
			lang = "cn"
		}
		return o.createProductLogstore(config, project, logstore, product, lang)
	}

	// @note hardcode for k8s audit, eg audit-cfc281c9c4ca548638a1aaa765d8f220d
	if strings.HasPrefix(logstore, "audit-") && len(logstore) == 39 {
		return o.createProductLogstore(config, project, logstore, "k8s-audit", "cn")
	}

	if project != o.project {
		if err := o.makesureProjectExist(config, project); err != nil {
			return err
		}
	}
	ok := false
	var err error
	for i := 0; i < *flags.LogOperationMaxRetryTimes; i++ {
		if ok, err = o.logClient.CheckLogstoreExist(project, logstore); err != nil {
			time.Sleep(time.Millisecond * 100)
		} else {
			break
		}
	}
	if ok {
		logger.Info(context.Background(), "logstore already exist, logstore", logstore)
		o.addLogstoreCache(project, logstore)
		return nil
	}
	ttl := 180
	if lifeCycle > 0 {
		ttl = lifeCycle
	}
	if shardCount <= 0 {
		shardCount = 2
	}
	// @note max init shard count limit : 10
	if shardCount > 10 {
		shardCount = 10
	}
	for i := 0; i < *flags.LogOperationMaxRetryTimes; i++ {
		err = o.logClient.CreateLogStore(project, logstore, ttl, shardCount, true, 32)
		if err != nil {
			time.Sleep(time.Millisecond * 100)
		} else {
			o.addLogstoreCache(project, logstore)
			logger.Info(context.Background(), "create logstore success, logstore", logstore)
			break
		}
	}
	annotations := GetAnnotationByObject(config, project, logstore, "", config.LogtailConfig.ConfigName, false)
	if err != nil {
		if k8s_event.GetEventRecorder() != nil {
			customErr := CustomErrorFromSlsSDKError(err)
			k8s_event.GetEventRecorder().SendErrorEventWithAnnotation(k8s_event.GetEventRecorder().GetObject(), GetAnnotationByError(annotations, customErr), k8s_event.CreateLogstore, "", fmt.Sprintf("create logstore failed, error: %s", err.Error()))
		}
		return err
	} else if k8s_event.GetEventRecorder() != nil {
		k8s_event.GetEventRecorder().SendNormalEventWithAnnotation(k8s_event.GetEventRecorder().GetObject(), annotations, k8s_event.CreateLogstore, "create logstore success")
	}

	// after create logstore success, wait 1 sec
	time.Sleep(time.Second)
	// use default k8s index
	index := createDefaultK8SIndex()
	// create index, create index do not return error
	for i := 0; i < *flags.LogOperationMaxRetryTimes; i++ {
		err = o.logClient.CreateIndex(project, logstore, *index)
		if err != nil {
			time.Sleep(time.Millisecond * 100)
		} else {
			break
		}
	}
	// create index always return nil
	if err == nil {
		logger.Info(context.Background(), "create index done, logstore", logstore)
	} else {
		logger.Warning(context.Background(), "CREATE_INDEX_ALARM", "create index done, logstore", logstore, "error", err)
	}
	return nil
}

func (o *operationWrapper) makesureProjectExist(config *AliyunLogConfigSpec, project string) error {
	ok := false
	var err error

	for i := 0; i < *flags.LogOperationMaxRetryTimes; i++ {
		if ok, err = o.logClient.CheckProjectExist(project); err != nil {
			time.Sleep(time.Millisecond * 1000)
		} else {
			break
		}
	}
	if ok {
		return nil
	}
	for i := 0; i < *flags.LogOperationMaxRetryTimes; i++ {
		_, err = o.logClient.CreateProject(project, "k8s log project, created by alibaba cloud log controller")
		if err != nil {
			time.Sleep(time.Millisecond * 1000)
		} else {
			break
		}
	}
	configName := ""
	logstore := ""
	if config != nil {
		configName = config.LogtailConfig.ConfigName
		logstore = config.Logstore
	}
	annotations := GetAnnotationByObject(config, project, logstore, "", configName, false)
	if err != nil {
		if k8s_event.GetEventRecorder() != nil {
			customErr := CustomErrorFromSlsSDKError(err)
			k8s_event.GetEventRecorder().SendErrorEventWithAnnotation(k8s_event.GetEventRecorder().GetObject(), GetAnnotationByError(annotations, customErr), k8s_event.CreateProject, "", fmt.Sprintf("create project failed, error: %s", err.Error()))
		}
	} else if k8s_event.GetEventRecorder() != nil {
		k8s_event.GetEventRecorder().SendNormalEventWithAnnotation(k8s_event.GetEventRecorder().GetObject(), annotations, k8s_event.CreateProject, "create project success")
	}
	return err
}

func (o *operationWrapper) makesureMachineGroupExist(project, machineGroup string) error {
	ok := false
	var err error

	for i := 0; i < *flags.LogOperationMaxRetryTimes; i++ {
		if ok, err = o.logClient.CheckMachineGroupExist(project, machineGroup); err != nil {
			time.Sleep(time.Millisecond * 100)
		} else {
			break
		}
	}
	if ok {
		return nil
	}
	m := &aliyunlog.MachineGroup{
		Name:          machineGroup,
		MachineIDType: aliyunlog.MachineIDTypeUserDefined,
		MachineIDList: []string{machineGroup},
	}
	for i := 0; i < *flags.LogOperationMaxRetryTimes; i++ {
		err = o.logClient.CreateMachineGroup(project, m)
		if err != nil {
			time.Sleep(time.Millisecond * 100)
		} else {
			break
		}
	}
	return err
}

// nolint:unused
func (o *operationWrapper) resetToken(accessKeyID, accessKeySecret, stsToken string) {
	o.logClient.ResetAccessKeyToken(accessKeyID, accessKeySecret, stsToken)
}

// nolint:unused
func (o *operationWrapper) deleteConfig(checkpoint *configCheckPoint) error {
	var err error
	for i := 0; i < *flags.LogOperationMaxRetryTimes; i++ {
		err = o.logClient.DeleteConfig(checkpoint.ProjectName, checkpoint.ConfigName)
		if err == nil {
			o.removeConfigCache(checkpoint.ProjectName, checkpoint.ConfigName)
			return nil
		}
		if sdkError, ok := err.(*aliyunlog.Error); ok {
			if sdkError.HTTPCode == 404 {
				// no project or no config, return true
				o.removeConfigCache(checkpoint.ProjectName, checkpoint.ConfigName)
				return nil
			}
		}
		// sleep 100 ms
		time.Sleep(time.Millisecond * 100)
	}
	if err != nil {
		return fmt.Errorf("DeleteConfig error, config : %s, error : %s", checkpoint.ConfigName, err.Error())
	}
	return err
}

func (o *operationWrapper) updateConfig(config *AliyunLogConfigSpec) error {
	return o.updateConfigInner(config)
}

// nolint:unused
func (o *operationWrapper) createConfig(config *AliyunLogConfigSpec) error {
	return o.updateConfigInner(config)
}

func checkFileConfigChanged(filePath, filePattern, includeEnv, includeLabel string, serverConfigDetail map[string]interface{}) bool {
	serverFilePath, _ := util.InterfaceToString(serverConfigDetail["logPath"])
	serverFilePattern, _ := util.InterfaceToString(serverConfigDetail["filePattern"])
	serverIncludeEnv, _ := util.InterfaceToJSONString(serverConfigDetail["dockerIncludeEnv"])
	serverIncludeLabel, _ := util.InterfaceToJSONString(serverConfigDetail["dockerIncludeLabel"])
	return filePath != serverFilePath ||
		filePattern != serverFilePattern ||
		includeEnv != serverIncludeEnv ||
		includeLabel != serverIncludeLabel
}

// nolint:govet,ineffassign
func (o *operationWrapper) updateConfigInner(config *AliyunLogConfigSpec) error {
	project := o.project
	if len(config.Project) != 0 {
		project = config.Project
	}
	logstore := config.Logstore
	shardCount := 0
	if config.ShardCount != nil {
		shardCount = int(*config.ShardCount)
	}
	lifeCycle := 0
	if config.LifeCycle != nil {
		lifeCycle = int(*config.LifeCycle)
	}
	err := o.makesureLogstoreExist(config, project, logstore, shardCount, lifeCycle, config.ProductCode, config.ProductLang)
	if err != nil {
		return fmt.Errorf("Create logconfig error when update config, config : %s, error : %s", config.LogtailConfig.ConfigName, err.Error())
	}

	// make config
	inputType := config.LogtailConfig.InputType
	if len(inputType) == 0 {
		inputType = aliyunlog.InputTypeFile
	}
	aliyunLogConfig := &aliyunlog.LogConfig{
		Name:        config.LogtailConfig.ConfigName,
		InputDetail: addNecessaryInputConfigField(config.LogtailConfig.LogtailConfig),
		InputType:   inputType,
		OutputType:  aliyunlog.OutputTypeLogService,
		OutputDetail: aliyunlog.OutputDetail{
			ProjectName:  project,
			LogStoreName: logstore,
		},
	}

	logger.Info(context.Background(), "create or update config", config.LogtailConfig.ConfigName, "detail", *aliyunLogConfig)

	ok := false
	// if o.configCacheExists(project, config.LogtailConfig.ConfigName) {
	// 	ok = true
	// } else {
	// 	// check if config exists
	// 	for i := 0; i < *LogOperationMaxRetryTimes; i++ {
	// 		ok, err = o.logClient.CheckConfigExist(project, config.LogtailConfig.ConfigName)
	// 		if err == nil {
	// 			break
	// 		}
	// 	}
	// }

	var serverConfig *aliyunlog.LogConfig
	for i := 0; i < *flags.LogOperationMaxRetryTimes; i++ {
		serverConfig, err = o.logClient.GetConfig(project, config.LogtailConfig.ConfigName)
		if err != nil {
			if slsErr, ok := err.(*aliyunlog.Error); ok {
				if slsErr.Code == "ConfigNotExist" {
					ok = false
					break
				}
			}
		} else {
			ok = true
			break
		}
	}

	logger.Info(context.Background(), "get config", config.LogtailConfig.ConfigName, "result", ok)

	// create config or update config
	if ok && serverConfig != nil {
		if config.SimpleConfig {
			configDetail, ok := serverConfig.InputDetail.(map[string]interface{})
			// only update file type's filePattern and logPath
			if config.LogtailConfig.InputType == "file" && serverConfig.InputType == "file" && ok {
				filePattern, _ := util.InterfaceToString(config.LogtailConfig.LogtailConfig["filePattern"])
				logPath, _ := util.InterfaceToString(config.LogtailConfig.LogtailConfig["logPath"])
				includeEnv, _ := util.InterfaceToJSONString(config.LogtailConfig.LogtailConfig["dockerIncludeEnv"])
				includeLabel, _ := util.InterfaceToJSONString(config.LogtailConfig.LogtailConfig["dockerIncludeLabel"])

				if len(filePattern) > 0 && len(logPath) > 0 {

					if checkFileConfigChanged(logPath, filePattern, includeEnv, includeLabel, configDetail) {
						logger.Info(context.Background(), "file config changed, old", configDetail, "new", config.LogtailConfig.LogtailConfig)
						configDetail["logPath"] = logPath
						configDetail["filePattern"] = filePattern
						configDetail["dockerIncludeEnv"] = config.LogtailConfig.LogtailConfig["dockerIncludeEnv"]
						if _, hasLabel := configDetail["dockerIncludeLabel"]; hasLabel {
							if _, hasLocalLabel := config.LogtailConfig.LogtailConfig["dockerIncludeLabel"]; hasLocalLabel {
								configDetail["dockerIncludeLabel"] = config.LogtailConfig.LogtailConfig["dockerIncludeLabel"]
							}
						}
						serverConfig.InputDetail = configDetail
						// update config
						for i := 0; i < *flags.LogOperationMaxRetryTimes; i++ {
							err = o.logClient.UpdateConfig(project, serverConfig)
							if err == nil {
								break
							}
						}

						annotations := GetAnnotationByObject(config, project, logstore, "", config.LogtailConfig.ConfigName, true)
						if err != nil {
							if k8s_event.GetEventRecorder() != nil {
								customErr := CustomErrorFromSlsSDKError(err)
								k8s_event.GetEventRecorder().SendErrorEventWithAnnotation(k8s_event.GetEventRecorder().GetObject(), GetAnnotationByError(annotations, customErr), k8s_event.UpdateConfig, "", fmt.Sprintf("update config failed, error: %s", err.Error()))
							}
						} else {
							if k8s_event.GetEventRecorder() != nil {
								k8s_event.GetEventRecorder().SendNormalEventWithAnnotation(k8s_event.GetEventRecorder().GetObject(), annotations, k8s_event.UpdateConfig, "update config success")
							}
						}

					} else {
						logger.Info(context.Background(), "file config not changed", "skip update")
					}
				}
			}
			if config.LogtailConfig.InputType != serverConfig.InputType && ok {
				// force update
				logger.Info(context.Background(), "config input type change from", serverConfig.InputType,
					"to", config.LogtailConfig.InputType, "force update")
				// update config
				for i := 0; i < *flags.LogOperationMaxRetryTimes; i++ {
					err = o.logClient.UpdateConfig(project, aliyunLogConfig)
					if err == nil {
						break
					}
				}
			}
			logger.Info(context.Background(), "config updated, server config", *serverConfig, "local config", *config)
		}

	} else {
		for i := 0; i < *flags.LogOperationMaxRetryTimes; i++ {
			err = o.logClient.CreateConfig(project, aliyunLogConfig)
			if err == nil {
				break
			}
		}
		annotations := GetAnnotationByObject(config, project, logstore, "", config.LogtailConfig.ConfigName, true)
		if err != nil {
			if k8s_event.GetEventRecorder() != nil {
				customErr := CustomErrorFromSlsSDKError(err)
				k8s_event.GetEventRecorder().SendErrorEventWithAnnotation(k8s_event.GetEventRecorder().GetObject(), GetAnnotationByError(annotations, customErr), k8s_event.UpdateConfig, "", fmt.Sprintf("update config failed, error: %s", err.Error()))
			}
		} else if k8s_event.GetEventRecorder() != nil {
			k8s_event.GetEventRecorder().SendNormalEventWithAnnotation(k8s_event.GetEventRecorder().GetObject(), annotations, k8s_event.UpdateConfig, "update config success")
		}
	}
	if err != nil {
		return fmt.Errorf("UpdateConfig error, config : %s, error : %s", config.LogtailConfig.ConfigName, err.Error())
	}
	logger.Info(context.Background(), "create or update config success", config.LogtailConfig.ConfigName)
	// check if config is in the machine group
	// only check when create config
	var machineGroup string
	if len(config.MachineGroups) > 0 {
		machineGroup = config.MachineGroups[0]
	} else {
		machineGroup = *flags.DefaultLogMachineGroup
	}
	_ = o.makesureMachineGroupExist(project, machineGroup)
	if ok {
		var machineGroups []string
		for i := 0; i < *flags.LogOperationMaxRetryTimes; i++ {
			machineGroups, err = o.logClient.GetAppliedMachineGroups(project, config.LogtailConfig.ConfigName)
			if err == nil {
				break
			}
		}
		if err != nil {
			return fmt.Errorf("GetAppliedMachineGroups error, config : %s, error : %s", config.LogtailConfig.ConfigName, err.Error())
		}
		ok = false
		for _, key := range machineGroups {
			if key == machineGroup {
				ok = true
				break
			}
		}
	}
	// apply config to the machine group
	for i := 0; i < *flags.LogOperationMaxRetryTimes; i++ {
		err = o.logClient.ApplyConfigToMachineGroup(project, config.LogtailConfig.ConfigName, machineGroup)
		if err == nil {
			break
		}
	}
	if err != nil {
		return fmt.Errorf("ApplyConfigToMachineGroup error, config : %s, machine group : %s, error : %s", config.LogtailConfig.ConfigName, machineGroup, err.Error())
	}
	logger.Info(context.Background(), "apply config to machine group success", config.LogtailConfig.ConfigName, "group", machineGroup)
	o.addConfigCache(project, config.LogtailConfig.ConfigName)
	return nil
}
