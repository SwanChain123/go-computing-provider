package computing

import (
	"context"
	"encoding/json"
	stErr "errors"
	"fmt"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/ethclient"
	"github.com/filswan/go-mcs-sdk/mcs/api/common/logs"
	"github.com/gin-gonic/gin"
	"github.com/gomodule/redigo/redis"
	"github.com/google/uuid"
	"github.com/gorilla/websocket"
	"github.com/joho/godotenv"
	"github.com/swanchain/go-computing-provider/account"
	"github.com/swanchain/go-computing-provider/build"
	"github.com/swanchain/go-computing-provider/conf"
	"github.com/swanchain/go-computing-provider/constants"
	"github.com/swanchain/go-computing-provider/internal/models"
	"github.com/swanchain/go-computing-provider/util"
	"github.com/swanchain/go-computing-provider/wallet"
	"io"
	batchv1 "k8s.io/api/batch/v1"
	coreV1 "k8s.io/api/core/v1"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metaV1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"math/rand"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"
)

func GetCpInfo(c *gin.Context) {
	var info struct {
		NodeId       string `json:"node_id"`
		MultiAddress string `json:"multi_address"`
		UbiTask      int    `json:"ubi_task"`
	}

	cpPath, exit := os.LookupEnv("CP_PATH")
	if !exit {
		return
	}

	info.NodeId = GetNodeId(cpPath)
	info.MultiAddress = conf.GetConfig().API.MultiAddress
	info.UbiTask = 0
	if conf.GetConfig().UBI.UbiTask {
		info.UbiTask = 1
	}
	c.JSON(http.StatusOK, util.CreateSuccessResponse(info))
}

func GetServiceProviderInfo(c *gin.Context) {
	info := new(models.HostInfo)
	info.SwanProviderVersion = build.UserVersion()
	info.OperatingSystem = runtime.GOOS
	info.Architecture = runtime.GOARCH
	info.CPUCores = runtime.NumCPU()
	c.JSON(http.StatusOK, util.CreateSuccessResponse(info))
}

func ReceiveJob(c *gin.Context) {
	var jobData models.JobData
	if err := c.ShouldBindJSON(&jobData); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	logs.GetLogger().Infof("Job received Data: %+v", jobData)

	if conf.GetConfig().HUB.VerifySign {
		if len(jobData.NodeIdJobSourceUriSignature) == 0 {
			c.JSON(http.StatusBadRequest, util.CreateErrorResponse(util.SpaceSignatureError, "missing node_id_job_source_uri_signature field"))
			return
		}
		cpRepoPath, _ := os.LookupEnv("CP_PATH")
		nodeID := GetNodeId(cpRepoPath)

		signature, err := verifySignatureForHub(conf.GetConfig().HUB.OrchestratorPk, fmt.Sprintf("%s%s", nodeID, jobData.JobSourceURI), jobData.NodeIdJobSourceUriSignature)
		if err != nil {
			logs.GetLogger().Errorf("verifySignature for space job failed, error: %+v", err)
			c.JSON(http.StatusInternalServerError, util.CreateErrorResponse(util.ServerError, "verify sign data failed"))
			return
		}

		logs.GetLogger().Infof("space job sign verifing, task_id: %s,  verify: %v", jobData.TaskUUID, signature)
		if !signature {
			c.JSON(http.StatusInternalServerError, util.CreateErrorResponse(util.SpaceSignatureError, "signature verify failed"))
			return
		}
	}

	available, gpuProductName, err := checkResourceAvailableForSpace(jobData.JobSourceURI)
	if err != nil {
		logs.GetLogger().Errorf("check job resource failed, error: %+v", err)
		c.JSON(http.StatusInternalServerError, util.CreateErrorResponse(util.CheckResourcesError))
		return
	}

	if !available {
		logs.GetLogger().Warnf(" task id: %s, name: %s, not found a resources available", jobData.TaskUUID, jobData.Name)
		c.JSON(http.StatusInternalServerError, util.CreateErrorResponse(util.CheckAvailableResources))
		return
	}

	var hostName string
	var logHost string
	prefixStr := generateString(10)
	if strings.HasPrefix(conf.GetConfig().API.Domain, ".") {
		hostName = prefixStr + conf.GetConfig().API.Domain
		logHost = "log" + conf.GetConfig().API.Domain
	} else {
		hostName = strings.Join([]string{prefixStr, conf.GetConfig().API.Domain}, ".")
		logHost = "log." + conf.GetConfig().API.Domain
	}

	if _, err = celeryService.DelayTask(constants.TASK_DEPLOY, jobData.JobSourceURI, hostName, jobData.Duration, jobData.UUID, jobData.TaskUUID, gpuProductName); err != nil {
		logs.GetLogger().Errorf("Failed sync delpoy task, error: %v", err)
		return
	}

	jobData.JobResultURI = fmt.Sprintf("https://%s", hostName)

	multiAddressSplit := strings.Split(conf.GetConfig().API.MultiAddress, "/")
	jobSourceUri := jobData.JobSourceURI
	spaceUuid := jobSourceUri[strings.LastIndex(jobSourceUri, "/")+1:]
	wsUrl := fmt.Sprintf("wss://%s:%s/api/v1/computing/lagrange/spaces/log?space_id=%s", logHost, multiAddressSplit[4], spaceUuid)
	jobData.BuildLog = wsUrl + "&type=build"
	jobData.ContainerLog = wsUrl + "&type=container"

	if err = submitJob(&jobData); err != nil {
		jobData.JobResultURI = ""
	}
	logs.GetLogger().Infof("submit job detail: %+v", jobData)
	c.JSON(http.StatusOK, jobData)
}

func submitJob(jobData *models.JobData) error {
	logs.GetLogger().Printf("submitting job...")
	oldMask := syscall.Umask(0)
	defer syscall.Umask(oldMask)

	fileCachePath := conf.GetConfig().MCS.FileCachePath
	folderPath := "jobs"
	jobDetailFile := filepath.Join(folderPath, uuid.NewString()+".json")
	os.MkdirAll(filepath.Join(fileCachePath, folderPath), os.ModePerm)
	taskDetailFilePath := filepath.Join(fileCachePath, jobDetailFile)

	jobData.Status = constants.BiddingSubmitted
	jobData.UpdatedAt = strconv.FormatInt(time.Now().Unix(), 10)
	bytes, err := json.Marshal(jobData)
	if err != nil {
		logs.GetLogger().Errorf("Failed Marshal JobData, error: %v", err)
		return err
	}
	if err = os.WriteFile(taskDetailFilePath, bytes, os.ModePerm); err != nil {
		logs.GetLogger().Errorf("Failed jobData write to file, error: %v", err)
		return err
	}

	storageService := NewStorageService()
	mcsOssFile, err := storageService.UploadFileToBucket(jobDetailFile, taskDetailFilePath, true)
	if err != nil {
		logs.GetLogger().Errorf("Failed upload file to bucket, error: %v", err)
		return err
	}
	logs.GetLogger().Infof("jobuuid: %s successfully submitted to IPFS", jobData.UUID)

	gatewayUrl, err := storageService.GetGatewayUrl()
	if err != nil {
		logs.GetLogger().Errorf("Failed get mcs ipfs gatewayUrl, error: %v", err)
		return err
	}
	jobData.JobResultURI = *gatewayUrl + "/ipfs/" + mcsOssFile.PayloadCid
	return nil
}

func RedeployJob(c *gin.Context) {
	var jobData models.JobData

	if err := c.ShouldBindJSON(&jobData); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	logs.GetLogger().Infof("redeploy Job received: %+v", jobData)

	available, gpuProductName, err := checkResourceAvailableForSpace(jobData.JobSourceURI)
	if err != nil {
		logs.GetLogger().Errorf("check job resource failed, error: %+v", err)
		c.JSON(http.StatusInternalServerError, util.CreateErrorResponse(util.CheckResourcesError))
		return
	}

	if !available {
		logs.GetLogger().Warnf(" task id: %s, name: %s, not found a resources available", jobData.TaskUUID, jobData.Name)
		c.JSON(http.StatusInternalServerError, util.CreateErrorResponse(util.CheckAvailableResources))
		return
	}

	var hostName string
	if jobData.JobResultURI != "" {
		resp, err := http.Get(jobData.JobResultURI)
		if err != nil {
			logs.GetLogger().Errorf("error making request to Space API: %+v", err)
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		defer func(Body io.ReadCloser) {
			err := Body.Close()
			if err != nil {
				logs.GetLogger().Errorf("error closed resp Space API: %+v", err)
			}
		}(resp.Body)
		logs.GetLogger().Infof("Space API response received. Response: %d", resp.StatusCode)
		if resp.StatusCode != http.StatusOK {
			logs.GetLogger().Errorf("space API response not OK. Status Code: %d", resp.StatusCode)
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		}

		var hostInfo struct {
			JobResultUri string `json:"job_result_uri"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&hostInfo); err != nil {
			logs.GetLogger().Errorf("error decoding Space API response JSON: %v", err)
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		hostName = strings.ReplaceAll(hostInfo.JobResultUri, "https://", "")
	} else {
		hostName = generateString(10) + conf.GetConfig().API.Domain
	}

	delayTask, err := celeryService.DelayTask(constants.TASK_DEPLOY, jobData.JobResultURI, hostName, jobData.Duration, jobData.UUID, jobData.TaskUUID, gpuProductName)
	if err != nil {
		logs.GetLogger().Errorf("Failed sync delpoy task, error: %v", err)
		return
	}
	logs.GetLogger().Infof("delayTask detail info: %+v", delayTask)

	go func() {
		result, err := delayTask.Get(180 * time.Second)
		if err != nil {
			logs.GetLogger().Errorf("Failed get sync task result, error: %v", err)
			return
		}
		logs.GetLogger().Infof("Job: %s, service running successfully, job_result_url: %s", jobData.JobResultURI, result.(string))
	}()

	jobData.JobResultURI = fmt.Sprintf("https://%s", hostName)
	if err = submitJob(&jobData); err != nil {
		jobData.JobResultURI = ""
	}
	c.JSON(http.StatusOK, jobData)
}

func ReNewJob(c *gin.Context) {
	var jobData struct {
		TaskUuid string `json:"task_uuid"`
		Duration int    `json:"duration"`
	}

	if err := c.ShouldBindJSON(&jobData); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	logs.GetLogger().Infof("renew Job received: %+v", jobData)

	if strings.TrimSpace(jobData.TaskUuid) == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "missing required field: task_uuid"})
		return
	}

	if jobData.Duration == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "missing required field: duration"})
		return
	}

	conn := redisPool.Get()
	prefix := constants.REDIS_SPACE_PREFIX + "*"
	keys, err := redis.Strings(conn.Do("KEYS", prefix))
	if err != nil {
		logs.GetLogger().Errorf("Failed get redis %s prefix, error: %+v", prefix, err)
		return
	}

	var spaceDetail models.CacheSpaceDetail
	for _, key := range keys {
		jobMetadata, err := RetrieveJobMetadata(key)
		if err != nil {
			logs.GetLogger().Errorf("Failed get redis key data, key: %s, error: %+v", key, err)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "query data failed"})
			return
		}
		if strings.EqualFold(jobMetadata.TaskUuid, jobData.TaskUuid) {
			spaceDetail = jobMetadata
			break
		}
	}

	redisKey := constants.REDIS_SPACE_PREFIX + spaceDetail.SpaceUuid
	leftTime := spaceDetail.ExpireTime - time.Now().Unix()
	if leftTime < 0 {
		c.JSON(http.StatusOK, map[string]string{
			"status":  "failed",
			"message": "The job was terminated due to its expiration date",
		})
		return
	} else {
		fullArgs := []interface{}{redisKey}
		fields := map[string]string{
			"wallet_address": spaceDetail.WalletAddress,
			"space_name":     spaceDetail.SpaceName,
			"expire_time":    strconv.Itoa(int(time.Now().Unix()) + int(leftTime) + jobData.Duration),
			"space_uuid":     spaceDetail.SpaceUuid,
			"job_uuid":       spaceDetail.JobUuid,
			"task_type":      spaceDetail.TaskType,
			"deploy_name":    spaceDetail.DeployName,
			"hardware":       spaceDetail.Hardware,
		}

		for key, val := range fields {
			fullArgs = append(fullArgs, key, val)
		}
		redisConn := redisPool.Get()
		defer redisConn.Close()

		redisConn.Do("HSET", fullArgs...)
		redisConn.Do("SET", spaceDetail.SpaceUuid, "wait-delete", "EX", int(leftTime)+jobData.Duration)
	}
	c.JSON(http.StatusOK, util.CreateSuccessResponse("success"))
}

func CancelJob(c *gin.Context) {
	taskUuid := c.Query("task_uuid")
	if taskUuid == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "task_uuid is required"})
		return
	}

	conn := redisPool.Get()
	prefix := constants.REDIS_SPACE_PREFIX + "*"
	keys, err := redis.Strings(conn.Do("KEYS", prefix))
	if err != nil {
		logs.GetLogger().Errorf("Failed get redis %s prefix, error: %+v", prefix, err)
		return
	}

	var jobDetail models.CacheSpaceDetail
	for _, key := range keys {
		jobMetadata, err := RetrieveJobMetadata(key)
		if err != nil {
			logs.GetLogger().Errorf("Failed get redis key data for , key: %s, error: %+v", key, err)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "query data failed"})
			return
		}
		if strings.EqualFold(jobMetadata.TaskUuid, taskUuid) {
			jobDetail = jobMetadata
			break
		}
	}

	if jobDetail.WalletAddress == "" {
		c.JSON(http.StatusOK, util.CreateSuccessResponse("deleted success"))
		return
	}
	go func() {
		defer func() {
			if err := recover(); err != nil {
				logs.GetLogger().Errorf("task_uuid: %s, delete space request failed, error: %+v", taskUuid, err)
				return
			}
		}()
		k8sNameSpace := constants.K8S_NAMESPACE_NAME_PREFIX + strings.ToLower(jobDetail.WalletAddress)
		deleteJob(k8sNameSpace, jobDetail.SpaceUuid)
	}()

	c.JSON(http.StatusOK, util.CreateSuccessResponse("deleted success"))
}

func StatisticalSources(c *gin.Context) {
	location, err := getLocation()
	if err != nil {
		logs.GetLogger().Error(err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed get location info"})
		return
	}

	k8sService := NewK8sService()
	statisticalSources, err := k8sService.StatisticalSources(context.TODO())
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, models.ClusterResource{
		Region:      location,
		ClusterInfo: statisticalSources,
	})
}

func GetSpaceLog(c *gin.Context) {
	spaceUuid := c.Query("space_id")
	logType := c.Query("type")
	if strings.TrimSpace(spaceUuid) == "" {
		logs.GetLogger().Errorf("get space log failed, space_id is empty: %s", spaceUuid)
		c.JSON(http.StatusBadRequest, gin.H{"error": "missing required field: space_id"})
		return
	}

	if strings.TrimSpace(logType) == "" {
		logs.GetLogger().Errorf("get space log failed, type is empty: %s", logType)
		c.JSON(http.StatusBadRequest, gin.H{"error": "missing required field: type"})
		return
	}

	if strings.TrimSpace(logType) != "build" && strings.TrimSpace(logType) != "container" {
		logs.GetLogger().Errorf("get space log failed, type is build or container, type:: %s", logType)
		c.JSON(http.StatusBadRequest, gin.H{"error": "missing required field: type"})
		return
	}

	redisKey := constants.REDIS_SPACE_PREFIX + spaceUuid
	spaceDetail, err := RetrieveJobMetadata(redisKey)
	if err != nil {
		logs.GetLogger().Error(err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "query data failed"})
		return
	}

	conn, err := upgrade.Upgrade(c.Writer, c.Request, nil)
	if err != nil {
		logs.GetLogger().Errorf("upgrading connection failed, error: %+v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "upgrading connection failed"})
		return
	}
	handleConnection(conn, spaceDetail, logType)
}

func DoProof(c *gin.Context) {
	var proofTask struct {
		Method    string `json:"method"`
		BlockData string `json:"block_data"`
		Exp       int64  `json:"exp"`
	}
	if err := c.ShouldBindJSON(&proofTask); err != nil {
		c.JSON(http.StatusBadRequest, util.CreateErrorResponse(util.JsonError))
		return
	}
	logs.GetLogger().Infof("do proof task received: %+v", proofTask)

	if strings.TrimSpace(proofTask.Method) == "" {
		c.JSON(http.StatusBadRequest, util.CreateErrorResponse(util.ProofParamError, "missing required field: method"))
		return
	}
	if proofTask.Method != "mine" {
		c.JSON(http.StatusBadRequest, util.CreateErrorResponse(util.ProofParamError, "method must be mine"))
		return
	}
	if proofTask.Exp < 0 || proofTask.Exp > 250 {
		c.JSON(http.StatusBadRequest, util.CreateErrorResponse(util.ProofParamError, "exp range is [0~250]"))
		return
	}

	k8sService := NewK8sService()
	job := &batchv1.Job{
		ObjectMeta: metaV1.ObjectMeta{
			Name: "proof-job-" + generateString(5),
		},
		Spec: batchv1.JobSpec{
			Template: v1.PodTemplateSpec{
				Spec: v1.PodSpec{
					Containers: []v1.Container{
						{
							Name:  "worker-container-" + generateString(5),
							Image: "filswan/worker-proof:v1.0",
							Env: []v1.EnvVar{
								{
									Name:  "METHOD",
									Value: proofTask.Method,
								},
								{
									Name:  "BLOCK_DATA",
									Value: proofTask.BlockData,
								},
								{
									Name:  "EXP",
									Value: strconv.Itoa(int(proofTask.Exp)),
								},
							},
						},
					},
					RestartPolicy: "Never",
				},
			},
			BackoffLimit:            new(int32),
			TTLSecondsAfterFinished: new(int32),
		},
	}

	*job.Spec.BackoffLimit = 1
	*job.Spec.TTLSecondsAfterFinished = 30

	createdJob, err := k8sService.k8sClient.BatchV1().Jobs(metaV1.NamespaceDefault).Create(context.TODO(), job, metaV1.CreateOptions{})
	if err != nil {
		logs.GetLogger().Errorf("Failed creating Pod: %v", err)
		c.JSON(http.StatusInternalServerError, util.CreateErrorResponse(util.ProofError))
		return
	}

	err = wait.PollImmediate(time.Second*3, time.Minute*5, func() (bool, error) {
		job, err := k8sService.k8sClient.BatchV1().Jobs(metaV1.NamespaceDefault).Get(context.Background(), createdJob.Name, metaV1.GetOptions{})
		if err != nil {
			logs.GetLogger().Errorf("Failed getting Job status: %v\n", err)
			return false, err
		}

		if job.Status.Succeeded > 0 {
			return true, nil
		}

		return false, nil
	})
	if err != nil {
		logs.GetLogger().Errorf("Failed waiting for Job completion: %v", err)
		c.JSON(http.StatusInternalServerError, util.CreateErrorResponse(util.ProofError))
		return
	}

	podList, err := k8sService.k8sClient.CoreV1().Pods(metaV1.NamespaceDefault).List(context.Background(), metaV1.ListOptions{
		LabelSelector: fmt.Sprintf("job-name=%s", createdJob.Name),
	})
	if err != nil {
		logs.GetLogger().Errorf("Error getting Pods for Job: %v\n", err)
		c.JSON(http.StatusInternalServerError, util.CreateErrorResponse(util.ProofError))
		return
	}

	if len(podList.Items) == 0 {
		logs.GetLogger().Errorf("No Pods found for Job.")
		c.JSON(http.StatusInternalServerError, util.CreateErrorResponse(util.ProofError))
		return
	}

	podName := podList.Items[0].Name
	podLog, err := k8sService.k8sClient.CoreV1().Pods(metaV1.NamespaceDefault).GetLogs(podName, &v1.PodLogOptions{}).Stream(context.Background())
	if err != nil {
		logs.GetLogger().Errorf("Failed gettingPod logs: %v", err)
		c.JSON(http.StatusInternalServerError, util.CreateErrorResponse(util.ProofReadLogError))
		return
	}
	defer podLog.Close()

	bytes, err := io.ReadAll(podLog)
	if err != nil {
		logs.GetLogger().Errorf("Failed gettingPod logs: %v", err)
		c.JSON(http.StatusInternalServerError, util.CreateErrorResponse(util.ProofReadLogError))
		return
	}
	c.JSON(http.StatusOK, util.CreateSuccessResponse(string(bytes)))
}

func DoUbiTask(c *gin.Context) {

	var ubiTask models.UBITaskReq
	if err := c.ShouldBindJSON(&ubiTask); err != nil {
		c.JSON(http.StatusBadRequest, util.CreateErrorResponse(util.JsonError))
		return
	}
	logs.GetLogger().Infof("receive ubi task received: %+v", ubiTask)

	if ubiTask.ID == 0 {
		c.JSON(http.StatusBadRequest, util.CreateErrorResponse(util.UbiTaskParamError, "missing required field: id"))
		return
	}
	if strings.TrimSpace(ubiTask.Name) == "" {
		c.JSON(http.StatusBadRequest, util.CreateErrorResponse(util.UbiTaskParamError, "missing required field: name"))
		return
	}

	if ubiTask.Type != 0 && ubiTask.Type != 1 {
		c.JSON(http.StatusBadRequest, util.CreateErrorResponse(util.UbiTaskParamError, "the value of task_type is 0 or 1"))
		return
	}
	if strings.TrimSpace(ubiTask.ZkType) == "" {
		c.JSON(http.StatusBadRequest, util.CreateErrorResponse(util.UbiTaskParamError, "missing required field: zk_type"))
		return
	}

	if strings.TrimSpace(ubiTask.InputParam) == "" {
		c.JSON(http.StatusBadRequest, util.CreateErrorResponse(util.UbiTaskParamError, "missing required field: input_param"))
		return
	}

	if strings.TrimSpace(ubiTask.Signature) == "" {
		c.JSON(http.StatusBadRequest, util.CreateErrorResponse(util.UbiTaskParamError, "missing required field: signature"))
		return
	}

	cpRepoPath, _ := os.LookupEnv("CP_PATH")
	nodeID := GetNodeId(cpRepoPath)

	signature, err := verifySignature(conf.GetConfig().UBI.UbiEnginePk, fmt.Sprintf("%s%d", nodeID, ubiTask.ID), ubiTask.Signature)
	if err != nil {
		logs.GetLogger().Errorf("verifySignature for ubi task failed, error: %+v", err)
		c.JSON(http.StatusInternalServerError, util.CreateErrorResponse(util.UbiTaskParamError, "sign data failed"))
		return
	}

	logs.GetLogger().Infof("ubi task sign verifing, task_id: %d, type: %s, verify: %v", ubiTask.ID, ubiTask.ZkType, signature)
	if !signature {
		c.JSON(http.StatusInternalServerError, util.CreateErrorResponse(util.UbiTaskParamError, "signature verify failed"))
		return
	}

	var gpuFlag = "0"
	var ubiTaskToRedis = new(models.CacheUbiTaskDetail)
	ubiTaskToRedis.TaskId = strconv.Itoa(ubiTask.ID)
	ubiTaskToRedis.TaskType = "CPU"
	if ubiTask.Type == 1 {
		ubiTaskToRedis.TaskType = "GPU"
		gpuFlag = "1"
	}
	ubiTaskToRedis.Status = constants.UBI_TASK_RECEIVED_STATUS
	ubiTaskToRedis.ZkType = ubiTask.ZkType
	ubiTaskToRedis.CreateTime = time.Now().Format("2006-01-02 15:04:05")
	SaveUbiTaskMetadata(ubiTaskToRedis)

	var envFilePath string
	envFilePath = filepath.Join(os.Getenv("CP_PATH"), "fil-c2.env")
	envVars, err := godotenv.Read(envFilePath)
	if err != nil {
		logs.GetLogger().Errorf("reading fil-c2-env.env failed, error: %v", err)
		return
	}

	c2GpuConfig := envVars["RUST_GPU_TOOLS_CUSTOM_GPU"]
	c2GpuConfig = convertGpuName(strings.TrimSpace(c2GpuConfig))
	nodeName, architecture, needCpu, needMemory, needStorage, err := checkResourceAvailableForUbi(ubiTask.Type, c2GpuConfig, ubiTask.Resource)
	if err != nil {
		ubiTaskToRedis.Status = constants.UBI_TASK_FAILED_STATUS
		SaveUbiTaskMetadata(ubiTaskToRedis)
		logs.GetLogger().Errorf("check resource failed, error: %v", err)
		c.JSON(http.StatusInternalServerError, util.CreateErrorResponse(util.CheckResourcesError))
		return
	}

	if nodeName == "" {
		ubiTaskToRedis.Status = constants.UBI_TASK_FAILED_STATUS
		SaveUbiTaskMetadata(ubiTaskToRedis)
		logs.GetLogger().Warnf("ubi task id: %d, type: %s, not found a resources available", ubiTask.ID, ubiTaskToRedis.TaskType)
		c.JSON(http.StatusInternalServerError, util.CreateErrorResponse(util.CheckAvailableResources))
		return
	}

	var ubiTaskImage string
	if architecture == constants.CPU_AMD {
		ubiTaskImage = build.UBITaskImageAmdCpu
		if gpuFlag == "1" {
			ubiTaskImage = build.UBITaskImageAmdGpu
		}
	} else if architecture == constants.CPU_INTEL {
		ubiTaskImage = build.UBITaskImageIntelCpu
		if gpuFlag == "1" {
			ubiTaskImage = build.UBITaskImageIntelGpu
		}
	}

	mem := strings.Split(strings.TrimSpace(ubiTask.Resource.Memory), " ")[1]
	memUnit := strings.ReplaceAll(mem, "B", "")
	disk := strings.Split(strings.TrimSpace(ubiTask.Resource.Storage), " ")[1]
	diskUnit := strings.ReplaceAll(disk, "B", "")
	memQuantity, err := resource.ParseQuantity(fmt.Sprintf("%d%s", needMemory, memUnit))
	if err != nil {
		ubiTaskToRedis.Status = constants.UBI_TASK_FAILED_STATUS
		SaveUbiTaskMetadata(ubiTaskToRedis)
		logs.GetLogger().Error("get memory failed, error: %+v", err)
		return
	}

	storageQuantity, err := resource.ParseQuantity(fmt.Sprintf("%d%s", needStorage, diskUnit))
	if err != nil {
		ubiTaskToRedis.Status = constants.UBI_TASK_FAILED_STATUS
		SaveUbiTaskMetadata(ubiTaskToRedis)
		logs.GetLogger().Error("get storage failed, error: %+v", err)
		return
	}

	maxMemQuantity, err := resource.ParseQuantity(fmt.Sprintf("%d%s", needMemory*2, memUnit))
	if err != nil {
		ubiTaskToRedis.Status = constants.UBI_TASK_FAILED_STATUS
		SaveUbiTaskMetadata(ubiTaskToRedis)
		logs.GetLogger().Error("get memory failed, error: %+v", err)
		return
	}

	maxStorageQuantity, err := resource.ParseQuantity(fmt.Sprintf("%d%s", needStorage*2, diskUnit))
	if err != nil {
		ubiTaskToRedis.Status = constants.UBI_TASK_FAILED_STATUS
		SaveUbiTaskMetadata(ubiTaskToRedis)
		logs.GetLogger().Error("get storage failed, error: %+v", err)
		return
	}

	resourceRequirements := coreV1.ResourceRequirements{
		Limits: coreV1.ResourceList{
			coreV1.ResourceCPU:              *resource.NewQuantity(needCpu*2, resource.DecimalSI),
			coreV1.ResourceMemory:           maxMemQuantity,
			coreV1.ResourceEphemeralStorage: maxStorageQuantity,
			"nvidia.com/gpu":                resource.MustParse(gpuFlag),
		},
		Requests: coreV1.ResourceList{
			coreV1.ResourceCPU:              *resource.NewQuantity(needCpu, resource.DecimalSI),
			coreV1.ResourceMemory:           memQuantity,
			coreV1.ResourceEphemeralStorage: storageQuantity,
			"nvidia.com/gpu":                resource.MustParse(gpuFlag),
		},
	}

	go func() {
		var namespace = "ubi-task-" + strconv.Itoa(ubiTask.ID)
		var err error
		defer func() {
			key := constants.REDIS_UBI_C2_PERFIX + strconv.Itoa(ubiTask.ID)
			ubiTaskRun, _ := RetrieveUbiTaskMetadata(key)
			if ubiTaskRun.TaskId == "" {
				ubiTaskRun = new(models.CacheUbiTaskDetail)
				ubiTaskRun.TaskId = ubiTaskToRedis.TaskId
				ubiTaskRun.TaskType = ubiTaskToRedis.TaskType
				ubiTaskRun.ZkType = ubiTask.ZkType
				ubiTaskRun.CreateTime = ubiTaskToRedis.CreateTime
			}

			if err == nil {
				ubiTaskRun.Status = constants.UBI_TASK_RUNNING_STATUS
			} else {
				ubiTaskRun.Status = constants.UBI_TASK_FAILED_STATUS
				k8sService := NewK8sService()
				k8sService.k8sClient.CoreV1().Namespaces().Delete(context.TODO(), namespace, metaV1.DeleteOptions{})
			}
			SaveUbiTaskMetadata(ubiTaskRun)
		}()

		k8sService := NewK8sService()
		if _, err = k8sService.GetNameSpace(context.TODO(), namespace, metaV1.GetOptions{}); err != nil {
			if errors.IsNotFound(err) {
				k8sNamespace := &v1.Namespace{
					ObjectMeta: metaV1.ObjectMeta{
						Name: namespace,
					},
				}
				_, err = k8sService.CreateNameSpace(context.TODO(), k8sNamespace, metaV1.CreateOptions{})
				if err != nil {
					logs.GetLogger().Errorf("create namespace failed, error: %v", err)
					return
				}
			}
		}

		receiveUrl := fmt.Sprintf("%s:%d/api/v1/computing/cp/receive/ubi", k8sService.GetAPIServerEndpoint(), conf.GetConfig().API.Port)
		execCommand := []string{"ubi-bench", "c2"}
		JobName := strings.ToLower(ubiTask.ZkType) + "-" + strconv.Itoa(ubiTask.ID)

		filC2Param := envVars["FIL_PROOFS_PARAMETER_CACHE"]
		if gpuFlag == "0" {
			delete(envVars, "RUST_GPU_TOOLS_CUSTOM_GPU")
			envVars["BELLMAN_NO_GPU"] = "1"
		}

		delete(envVars, "FIL_PROOFS_PARAMETER_CACHE")
		var useEnvVars []v1.EnvVar
		for k, v := range envVars {
			useEnvVars = append(useEnvVars, v1.EnvVar{
				Name:  k,
				Value: v,
			})
		}

		useEnvVars = append(useEnvVars, v1.EnvVar{
			Name:  "RECEIVE_PROOF_URL",
			Value: receiveUrl,
		},
			v1.EnvVar{
				Name:  "TASKID",
				Value: strconv.Itoa(ubiTask.ID),
			},
			v1.EnvVar{
				Name:  "TASK_TYPE",
				Value: strconv.Itoa(ubiTask.Type),
			},
			v1.EnvVar{
				Name:  "ZK_TYPE",
				Value: ubiTask.ZkType,
			},
			v1.EnvVar{
				Name:  "NAME_SPACE",
				Value: namespace,
			},
			v1.EnvVar{
				Name:  "PARAM_URL",
				Value: ubiTask.InputParam,
			},
		)

		job := &batchv1.Job{
			ObjectMeta: metaV1.ObjectMeta{
				Name:      JobName,
				Namespace: namespace,
			},
			Spec: batchv1.JobSpec{
				Template: v1.PodTemplateSpec{
					Spec: v1.PodSpec{
						NodeName: nodeName,
						Containers: []v1.Container{
							{
								Name:  JobName + generateString(5),
								Image: ubiTaskImage,
								Env:   useEnvVars,
								VolumeMounts: []v1.VolumeMount{
									{
										Name:      "proof-params",
										MountPath: "/var/tmp/filecoin-proof-parameters",
									},
								},
								Command:         execCommand,
								Resources:       resourceRequirements,
								ImagePullPolicy: coreV1.PullIfNotPresent,
							},
						},
						Volumes: []v1.Volume{
							{
								Name: "proof-params",
								VolumeSource: v1.VolumeSource{
									HostPath: &v1.HostPathVolumeSource{
										Path: filC2Param,
									},
								},
							},
						},
						RestartPolicy: "Never",
					},
				},
				BackoffLimit:            new(int32),
				TTLSecondsAfterFinished: new(int32),
			},
		}

		*job.Spec.BackoffLimit = 1
		*job.Spec.TTLSecondsAfterFinished = 120

		if _, err = k8sService.k8sClient.BatchV1().Jobs(namespace).Create(context.TODO(), job, metaV1.CreateOptions{}); err != nil {
			logs.GetLogger().Errorf("Failed creating ubi task job: %v", err)
			return
		}
	}()

	c.JSON(http.StatusOK, util.CreateSuccessResponse("success"))
}

func ReceiveUbiProof(c *gin.Context) {
	var c2Proof struct {
		TaskId    string `json:"task_id"`
		TaskType  string `json:"task_type"`
		Proof     string `json:"proof"`
		ZkType    string `json:"zk_type"`
		NameSpace string `json:"name_space"`
	}

	var submitUBIProofTx string
	var err error
	defer func() {
		key := constants.REDIS_UBI_C2_PERFIX + c2Proof.TaskId
		ubiTask, _ := RetrieveUbiTaskMetadata(key)
		if err == nil {
			ubiTask.Status = constants.UBI_TASK_SUCCESS_STATUS
		} else {
			ubiTask.Status = constants.UBI_TASK_FAILED_STATUS
		}
		ubiTask.Tx = submitUBIProofTx
		SaveUbiTaskMetadata(ubiTask)
		if strings.TrimSpace(c2Proof.NameSpace) != "" {
			k8sService := NewK8sService()
			k8sService.k8sClient.CoreV1().Namespaces().Delete(context.TODO(), c2Proof.NameSpace, metaV1.DeleteOptions{})
		}
	}()

	if err := c.ShouldBindJSON(&c2Proof); err != nil {
		c.JSON(http.StatusBadRequest, util.CreateErrorResponse(util.JsonError))
		return
	}
	logs.GetLogger().Infof("task_id: %s, C2 proof out received: %+v", c2Proof.TaskId, c2Proof)

	chainUrl, err := conf.GetRpcByName(conf.DefaultRpc)
	if err != nil {
		logs.GetLogger().Errorf("get rpc url failed, error: %v,", err)
		return
	}

	client, err := ethclient.Dial(chainUrl)
	if err != nil {
		logs.GetLogger().Errorf("dial rpc connect failed, error: %v,", err)
		return
	}
	defer client.Close()

	cpStub, err := account.NewAccountStub(client)
	if err != nil {
		logs.GetLogger().Errorf("create ubi task client failed, error: %v,", err)
		return
	}
	cpAccount, err := cpStub.GetCpAccountInfo()
	if err != nil {
		logs.GetLogger().Errorf("get account info failed, error: %v,", err)
		return
	}

	localWallet, err := wallet.SetupWallet(wallet.WalletRepo)
	if err != nil {
		logs.GetLogger().Errorf("setup wallet failed, error: %v,", err)
		return
	}

	ki, err := localWallet.FindKey(cpAccount.OwnerAddress)
	if err != nil || ki == nil {
		logs.GetLogger().Errorf("the address: %s, private key %v,", conf.GetConfig().HUB.WalletAddress, wallet.ErrKeyInfoNotFound)
		return
	}

	accountStub, err := account.NewAccountStub(client, account.WithCpPrivateKey(ki.PrivateKey))
	if err != nil {
		logs.GetLogger().Errorf("create ubi task client failed, error: %v,", err)
		return
	}

	taskType, err := strconv.ParseUint(c2Proof.TaskType, 10, 8)
	if err != nil {
		logs.GetLogger().Errorf("conversion to uint8 error: %v", err)
		return
	}

	submitUBIProofTx, err = accountStub.SubmitUBIProof(c2Proof.TaskId, uint8(taskType), c2Proof.ZkType, c2Proof.Proof)
	if err != nil {
		logs.GetLogger().Errorf("submit ubi proof tx failed, error: %v,", err)
		return
	}

	fmt.Printf("submitUBIProofTx: %s", submitUBIProofTx)
	c.JSON(http.StatusOK, util.CreateSuccessResponse("success"))
}

func handleConnection(conn *websocket.Conn, spaceDetail models.CacheSpaceDetail, logType string) {
	client := NewWsClient(conn)

	if logType == "build" {
		buildLogPath := filepath.Join("build", spaceDetail.WalletAddress, "spaces", spaceDetail.SpaceName, BuildFileName)
		if _, err := os.Stat(buildLogPath); err != nil {
			client.HandleLogs(strings.NewReader("This space is deployed starting from a image."))
		} else {
			logFile, _ := os.Open(buildLogPath)
			defer logFile.Close()
			client.HandleLogs(logFile)
		}
	} else if logType == "container" {
		k8sNameSpace := constants.K8S_NAMESPACE_NAME_PREFIX + strings.ToLower(spaceDetail.WalletAddress)

		k8sService := NewK8sService()
		pods, err := k8sService.k8sClient.CoreV1().Pods(k8sNameSpace).List(context.TODO(), metaV1.ListOptions{
			LabelSelector: fmt.Sprintf("lad_app=%s", spaceDetail.SpaceUuid),
		})
		if err != nil {
			logs.GetLogger().Errorf("Error listing Pods: %v", err)
			return
		}

		if len(pods.Items) > 0 {
			line := int64(1000)
			containerStatuses := pods.Items[0].Status.ContainerStatuses
			lastIndex := len(containerStatuses) - 1
			req := k8sService.k8sClient.CoreV1().Pods(k8sNameSpace).GetLogs(pods.Items[0].Name, &v1.PodLogOptions{
				Container:  containerStatuses[lastIndex].Name,
				Follow:     true,
				Timestamps: true,
				TailLines:  &line,
			})

			podLogs, err := req.Stream(context.Background())
			if err != nil {
				logs.GetLogger().Errorf("Error opening log stream: %v", err)
				return
			}
			defer podLogs.Close()

			client.HandleLogs(podLogs)
		}
	}
}

func DeploySpaceTask(jobSourceURI, hostName string, duration int, jobUuid string, taskUuid string, gpuProductName string) string {
	updateJobStatus(jobUuid, models.JobUploadResult)

	var success bool
	var spaceUuid string
	var walletAddress string
	defer func() {
		if !success {
			k8sNameSpace := constants.K8S_NAMESPACE_NAME_PREFIX + strings.ToLower(walletAddress)
			deleteJob(k8sNameSpace, spaceUuid)
		}

		if err := recover(); err != nil {
			logs.GetLogger().Errorf("deploy space task painc, error: %+v", err)
			return
		}
	}()

	spaceDetail, err := getSpaceDetail(jobSourceURI)
	if err != nil {
		logs.GetLogger().Errorln(err)
		return ""
	}

	walletAddress = spaceDetail.Data.Owner.PublicAddress
	spaceName := spaceDetail.Data.Space.Name
	spaceUuid = strings.ToLower(spaceDetail.Data.Space.Uuid)
	spaceHardware := spaceDetail.Data.Space.ActiveOrder.Config

	conn := redisPool.Get()
	fullArgs := []interface{}{constants.REDIS_SPACE_PREFIX + spaceUuid}
	fields := map[string]string{
		"wallet_address": walletAddress,
		"space_name":     spaceName,
		"expire_time":    strconv.Itoa(int(time.Now().Unix()) + duration),
		"space_uuid":     spaceUuid,
		"task_uuid":      taskUuid,
	}

	for key, val := range fields {
		fullArgs = append(fullArgs, key, val)
	}
	_, _ = conn.Do("HSET", fullArgs...)

	logs.GetLogger().Infof("uuid: %s, spaceName: %s, hardwareName: %s", spaceUuid, spaceName, spaceHardware.Description)
	if len(spaceHardware.Description) == 0 {
		return ""
	}

	deploy := NewDeploy(jobUuid, hostName, walletAddress, spaceHardware.Description, int64(duration), taskUuid)
	deploy.WithSpaceInfo(spaceUuid, spaceName)
	deploy.WithGpuProductName(gpuProductName)

	spacePath := filepath.Join("build", walletAddress, "spaces", spaceName)
	os.RemoveAll(spacePath)
	updateJobStatus(jobUuid, models.JobDownloadSource)
	containsYaml, yamlPath, imagePath, modelsSettingFile, err := BuildSpaceTaskImage(spaceUuid, spaceDetail.Data.Files)
	if err != nil {
		logs.GetLogger().Error(err)
		return ""
	}

	deploy.WithSpacePath(imagePath)
	if len(modelsSettingFile) > 0 {
		err := deploy.WithModelSettingFile(modelsSettingFile).ModelInferenceToK8s()
		if err != nil {
			logs.GetLogger().Error(err)
			return ""
		}
		return hostName
	}

	if containsYaml {
		deploy.WithYamlInfo(yamlPath).YamlToK8s()
	} else {
		imageName, dockerfilePath := BuildImagesByDockerfile(jobUuid, spaceUuid, spaceName, imagePath)
		deploy.WithDockerfile(imageName, dockerfilePath).DockerfileToK8s()
	}
	success = true

	return hostName
}

func deleteJob(namespace, spaceUuid string) error {
	deployName := constants.K8S_DEPLOY_NAME_PREFIX + spaceUuid
	serviceName := constants.K8S_SERVICE_NAME_PREFIX + spaceUuid
	ingressName := constants.K8S_INGRESS_NAME_PREFIX + spaceUuid

	logs.GetLogger().Infof("Start deleting space service, space_uuid: %s", spaceUuid)
	k8sService := NewK8sService()
	if err := k8sService.DeleteIngress(context.TODO(), namespace, ingressName); err != nil && !errors.IsNotFound(err) {
		logs.GetLogger().Errorf("Failed delete ingress, ingressName: %s, error: %+v", deployName, err)
		return err
	}

	if err := k8sService.DeleteService(context.TODO(), namespace, serviceName); err != nil && !errors.IsNotFound(err) {
		logs.GetLogger().Errorf("Failed delete service, serviceName: %s, error: %+v", serviceName, err)
		return err
	}

	dockerService := NewDockerService()
	deployImageIds, err := k8sService.GetDeploymentImages(context.TODO(), namespace, deployName)
	if err != nil && !errors.IsNotFound(err) {
		logs.GetLogger().Errorf("Failed get deploy imageIds, deployName: %s, error: %+v", deployName, err)
		return err
	}
	for _, imageId := range deployImageIds {
		dockerService.RemoveImage(imageId)
	}

	if err := k8sService.DeleteDeployment(context.TODO(), namespace, deployName); err != nil && !errors.IsNotFound(err) {
		logs.GetLogger().Errorf("Failed delete deployment, deployName: %s, error: %+v", deployName, err)
		return err
	}
	time.Sleep(6 * time.Second)

	if err := k8sService.DeleteDeployRs(context.TODO(), namespace, spaceUuid); err != nil && !errors.IsNotFound(err) {
		logs.GetLogger().Errorf("Failed delete ReplicaSetsController, spaceUuid: %s, error: %+v", spaceUuid, err)
		return err
	}

	if err := k8sService.DeletePod(context.TODO(), namespace, spaceUuid); err != nil && !errors.IsNotFound(err) {
		logs.GetLogger().Errorf("Failed delete pods, spaceUuid: %s, error: %+v", spaceUuid, err)
		return err
	}

	ticker := time.NewTicker(3 * time.Second)
	defer ticker.Stop()
	var count = 0
	for {
		<-ticker.C
		count++
		if count >= 20 {
			break
		}
		getPods, err := k8sService.GetPods(namespace, spaceUuid)
		if err != nil && !errors.IsNotFound(err) {
			logs.GetLogger().Errorf("Failed get pods form namespace, namepace: %s, error: %+v", namespace, err)
			continue
		}
		if !getPods {
			break
		}
	}

	logs.GetLogger().Infof("Deleted space service finished, space_uuid: %s", spaceUuid)
	return nil
}

func downloadModelUrl(namespace, spaceUuid, serviceIp string, podCmd []string) {
	k8sService := NewK8sService()
	podName, err := k8sService.WaitForPodRunning(namespace, spaceUuid, serviceIp)
	if err != nil {
		logs.GetLogger().Error(err)
		return
	}

	if err = k8sService.PodDoCommand(namespace, podName, "", podCmd); err != nil {
		logs.GetLogger().Error(err)
		return
	}
}

func updateJobStatus(jobUuid string, jobStatus models.JobStatus, url ...string) {
	go func() {
		if len(url) > 0 {
			deployingChan <- models.Job{
				Uuid:   jobUuid,
				Status: jobStatus,
				Count:  0,
				Url:    url[0],
			}
		} else {
			deployingChan <- models.Job{
				Uuid:   jobUuid,
				Status: jobStatus,
				Count:  0,
				Url:    "",
			}
		}
	}()
}

func getSpaceDetail(jobSourceURI string) (models.SpaceJSON, error) {
	resp, err := http.Get(jobSourceURI)
	if err != nil {
		return models.SpaceJSON{}, fmt.Errorf("error making request to Space API: %+v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return models.SpaceJSON{}, fmt.Errorf("space API response not OK. Status Code: %d", resp.StatusCode)
	}

	var spaceJson models.SpaceJSON
	if err := json.NewDecoder(resp.Body).Decode(&spaceJson); err != nil {
		return models.SpaceJSON{}, fmt.Errorf("error decoding Space API response JSON: %v", err)
	}
	return spaceJson, nil
}

func checkResourceAvailableForSpace(jobSourceURI string) (bool, string, error) {
	spaceDetail, err := getSpaceDetail(jobSourceURI)
	if err != nil {
		logs.GetLogger().Errorln(err)
		return false, "", err
	}

	taskType, hardwareDetail := getHardwareDetail(spaceDetail.Data.Space.ActiveOrder.Config.Description)
	k8sService := NewK8sService()

	activePods, err := k8sService.GetAllActivePod(context.TODO())
	if err != nil {
		return false, "", err
	}

	nodes, err := k8sService.k8sClient.CoreV1().Nodes().List(context.TODO(), metaV1.ListOptions{})
	if err != nil {
		return false, "", err
	}

	nodeGpuSummary, err := k8sService.GetNodeGpuSummary(context.TODO())
	if err != nil {
		logs.GetLogger().Errorf("Failed collect k8s gpu, error: %+v", err)
		return false, "", err
	}

	for _, node := range nodes.Items {
		nodeGpu, remainderResource, _ := GetNodeResource(activePods, &node)
		remainderCpu := remainderResource[ResourceCpu]
		remainderMemory := float64(remainderResource[ResourceMem] / 1024 / 1024 / 1024)
		remainderStorage := float64(remainderResource[ResourceStorage] / 1024 / 1024 / 1024)

		needCpu := hardwareDetail.Cpu.Quantity
		needMemory := float64(hardwareDetail.Memory.Quantity)
		needStorage := float64(hardwareDetail.Storage.Quantity)
		logs.GetLogger().Infof("checkResourceAvailableForSpace: needCpu: %d, needMemory: %.2f, needStorage: %.2f", needCpu, needMemory, needStorage)
		logs.GetLogger().Infof("checkResourceAvailableForSpace: remainingCpu: %d, remainingMemory: %.2f, remainingStorage: %.2f", remainderCpu, remainderMemory, remainderStorage)
		if needCpu <= remainderCpu && needMemory <= remainderMemory && needStorage <= remainderStorage {
			if taskType == "CPU" {
				return true, "", nil
			} else if taskType == "GPU" {
				var usedCount int64 = 0
				gpuName := strings.ToUpper(strings.ReplaceAll(hardwareDetail.Gpu.Unit, " ", "-"))
				logs.GetLogger().Infof("gpuName: %s, nodeGpu: %+v, nodeGpuSummary: %+v", gpuName, nodeGpu, nodeGpuSummary)
				var gpuProductName = ""
				for name, count := range nodeGpu {
					if strings.Contains(strings.ToUpper(name), gpuName) {
						usedCount = count
						gpuProductName = strings.ReplaceAll(strings.ToUpper(name), " ", "-")
						break
					}
				}

				for gName, gCount := range nodeGpuSummary[node.Name] {
					if strings.Contains(strings.ToUpper(gName), gpuName) {
						gpuProductName = strings.ReplaceAll(strings.ToUpper(gName), " ", "-")
						if usedCount+hardwareDetail.Gpu.Quantity <= gCount {
							return true, gpuProductName, nil
						}
					}
				}
				continue
			}
		}
	}
	return false, "", nil
}

func checkResourceAvailableForUbi(taskType int, gpuName string, resource *models.TaskResource) (string, string, int64, int64, int64, error) {
	k8sService := NewK8sService()
	activePods, err := k8sService.GetAllActivePod(context.TODO())
	if err != nil {
		return "", "", 0, 0, 0, err
	}

	nodes, err := k8sService.k8sClient.CoreV1().Nodes().List(context.TODO(), metaV1.ListOptions{})
	if err != nil {
		return "", "", 0, 0, 0, err
	}

	nodeGpuSummary, err := k8sService.GetNodeGpuSummary(context.TODO())
	if err != nil {
		logs.GetLogger().Errorf("Failed collect k8s gpu, error: %+v", err)
		return "", "", 0, 0, 0, err
	}

	needCpu, _ := strconv.ParseInt(resource.CPU, 10, 64)
	var needMemory, needStorage float64
	if len(strings.Split(strings.TrimSpace(resource.Memory), " ")) > 0 {
		needMemory, err = strconv.ParseFloat(strings.Split(strings.TrimSpace(resource.Memory), " ")[0], 64)

	}
	if len(strings.Split(strings.TrimSpace(resource.Storage), " ")) > 0 {
		needStorage, err = strconv.ParseFloat(strings.Split(strings.TrimSpace(resource.Storage), " ")[0], 64)
	}

	var nodeName, architecture string
	for _, node := range nodes.Items {
		if _, ok := node.Labels[constants.CPU_INTEL]; ok {
			architecture = constants.CPU_INTEL
		}
		if _, ok := node.Labels[constants.CPU_AMD]; ok {
			architecture = constants.CPU_AMD
		}

		nodeGpu, remainderResource, _ := GetNodeResource(activePods, &node)
		remainderCpu := remainderResource[ResourceCpu]
		remainderMemory := float64(remainderResource[ResourceMem] / 1024 / 1024 / 1024)
		remainderStorage := float64(remainderResource[ResourceStorage] / 1024 / 1024 / 1024)

		logs.GetLogger().Infof("checkResourceAvailableForUbi: needCpu: %d, needMemory: %.2f, needStorage: %.2f", needCpu, needMemory, needStorage)
		logs.GetLogger().Infof("checkResourceAvailableForUbi: remainingCpu: %d, remainingMemory: %.2f, remainingStorage: %.2f", remainderCpu, remainderMemory, remainderStorage)
		if needCpu <= remainderCpu && needMemory <= remainderMemory && needStorage <= remainderStorage {
			nodeName = node.Name
			if taskType == 0 {
				return nodeName, architecture, needCpu, int64(needMemory), int64(needStorage), nil
			} else if taskType == 1 {
				if gpuName == "" {
					nodeName = ""
					continue
				}
				gpuName = strings.ReplaceAll(gpuName, " ", "-")
				logs.GetLogger().Infof("gpuName: %s, nodeGpu: %+v, nodeGpuSummary: %+v", gpuName, nodeGpu, nodeGpuSummary)
				usedCount, ok := nodeGpu[gpuName]
				if !ok {
					usedCount = 0
				}

				if usedCount+1 <= nodeGpuSummary[node.Name][gpuName] {
					return nodeName, architecture, needCpu, int64(needMemory), int64(needStorage), nil
				}
				nodeName = ""
				continue
			}
		}
	}
	return nodeName, architecture, needCpu, int64(needMemory), int64(needStorage), nil
}

func generateString(length int) string {
	characters := "abcdefghijklmnopqrstuvwxyz"
	numbers := "0123456789"
	source := characters + numbers
	result := make([]byte, length)
	rand.Seed(time.Now().UnixNano())
	for i := 0; i < length; i++ {
		result[i] = source[rand.Intn(len(source))]
	}
	return string(result)
}

func getLocation() (string, error) {
	publicIpAddress, err := getLocalIPAddress()
	if err != nil {
		return "", err
	}
	logs.GetLogger().Infof("publicIpAddress: %s", publicIpAddress)

	resp, err := http.Get("http://ip-api.com/json/" + publicIpAddress)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}

	var ipInfo struct {
		Country     string `json:"country"`
		CountryCode string `json:"countryCode"`
		City        string `json:"city"`
		Region      string `json:"region"`
		RegionName  string `json:"regionName"`
	}
	if err = json.Unmarshal(body, &ipInfo); err != nil {
		return "", err
	}

	return strings.TrimSpace(ipInfo.RegionName) + "-" + ipInfo.CountryCode, nil
}

func getLocalIPAddress() (string, error) {
	req, err := http.NewRequest("GET", "https://ipapi.co/ip", nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/90.0.4430.212 Safari/537.36")

	client := http.DefaultClient
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	ipBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(ipBytes)), nil
}

var NotFoundRedisKey = stErr.New("not found redis key")

func RetrieveJobMetadata(key string) (models.CacheSpaceDetail, error) {
	redisConn := redisPool.Get()
	defer redisConn.Close()

	exist, err := redis.Int(redisConn.Do("EXISTS", key))
	if err != nil {
		return models.CacheSpaceDetail{}, err
	}
	if exist == 0 {
		return models.CacheSpaceDetail{}, NotFoundRedisKey
	}

	args := append([]interface{}{key}, "wallet_address", "space_name", "expire_time", "space_uuid", "job_uuid",
		"task_type", "deploy_name", "hardware", "url", "task_uuid")
	valuesStr, err := redis.Strings(redisConn.Do("HMGET", args...))
	if err != nil {
		logs.GetLogger().Errorf("Failed get redis key data, key: %s, error: %+v", key, err)
		return models.CacheSpaceDetail{}, err
	}

	var (
		walletAddress string
		spaceName     string
		expireTime    int64
		spaceUuid     string
		jobUuid       string
		taskType      string
		deployName    string
		hardware      string
		url           string
		taskUuid      string
	)

	if len(valuesStr) >= 3 {
		walletAddress = valuesStr[0]
		spaceName = valuesStr[1]
		expireTimeStr := valuesStr[2]
		spaceUuid = valuesStr[3]
		jobUuid = valuesStr[4]
		taskType = valuesStr[5]
		deployName = valuesStr[6]
		hardware = valuesStr[7]
		url = valuesStr[8]
		taskUuid = valuesStr[9]
		expireTime, err = strconv.ParseInt(strings.TrimSpace(expireTimeStr), 10, 64)
		if err != nil {
			logs.GetLogger().Errorf("Failed convert time str: [%s], error: %+v", expireTimeStr, err)
			return models.CacheSpaceDetail{}, err
		}
	}

	return models.CacheSpaceDetail{
		WalletAddress: walletAddress,
		SpaceName:     spaceName,
		SpaceUuid:     spaceUuid,
		ExpireTime:    expireTime,
		JobUuid:       jobUuid,
		TaskType:      taskType,
		DeployName:    deployName,
		Hardware:      hardware,
		Url:           url,
		TaskUuid:      taskUuid,
	}, nil
}

func SaveUbiTaskMetadata(ubiTask *models.CacheUbiTaskDetail) {
	redisConn := redisPool.Get()
	defer redisConn.Close()

	key := constants.REDIS_UBI_C2_PERFIX + ubiTask.TaskId
	redisConn.Do("DEL", redis.Args{}.AddFlat(key)...)

	fullArgs := []interface{}{key}
	fields := map[string]string{
		"task_id":     ubiTask.TaskId,
		"task_type":   ubiTask.TaskType,
		"zk_type":     ubiTask.ZkType,
		"tx":          ubiTask.Tx,
		"status":      ubiTask.Status,
		"create_time": ubiTask.CreateTime,
	}

	for k, val := range fields {
		fullArgs = append(fullArgs, k, val)
	}
	_, _ = redisConn.Do("HSET", fullArgs...)
}

func RetrieveUbiTaskMetadata(key string) (*models.CacheUbiTaskDetail, error) {
	redisConn := redisPool.Get()
	defer redisConn.Close()

	exist, err := redis.Int(redisConn.Do("EXISTS", key))
	if err != nil {
		return nil, err
	}
	if exist == 0 {
		return nil, NotFoundRedisKey
	}

	type CacheUbiTaskDetail struct {
		TaskId     string `json:"task_id"`
		TaskType   string `json:"task_type"`
		ZkType     string `json:"zk_type"`
		Tx         string `json:"tx"`
		Status     string `json:"status"`
		Reward     string `json:"reward"`
		CreateTime string `json:"create_time"`
	}

	args := append([]interface{}{key}, "task_id", "task_type", "zk_type", "tx", "status", "create_time")
	valuesStr, err := redis.Strings(redisConn.Do("HMGET", args...))
	if err != nil {
		logs.GetLogger().Errorf("Failed get redis key data, key: %s, error: %+v", key, err)
		return nil, err
	}

	var (
		taskId     string
		taskType   string
		zkType     string
		tx         string
		status     string
		createTime string
	)

	if len(valuesStr) >= 6 {
		taskId = valuesStr[0]
		taskType = valuesStr[1]
		zkType = valuesStr[2]
		tx = valuesStr[3]
		status = valuesStr[4]
		createTime = valuesStr[5]
	}

	return &models.CacheUbiTaskDetail{
		TaskId:     taskId,
		TaskType:   taskType,
		ZkType:     zkType,
		Tx:         tx,
		Status:     status,
		CreateTime: createTime,
	}, nil
}

func verifySignature(pubKStr, data, signature string) (bool, error) {
	sb, err := hexutil.Decode(signature)
	if err != nil {
		return false, err
	}
	hash := crypto.Keccak256Hash([]byte(data))
	sigPublicKeyECDSA, err := crypto.SigToPub(hash.Bytes(), sb)
	if err != nil {
		return false, err
	}
	pub := crypto.PubkeyToAddress(*sigPublicKeyECDSA).Hex()
	if pubKStr != pub {
		return false, err
	}
	return true, nil
}

func verifySignatureForHub(pubKStr string, message string, signedMessage string) (bool, error) {
	hashedMessage := []byte("\x19Ethereum Signed Message:\n" + strconv.Itoa(len(message)) + message)
	hash := crypto.Keccak256Hash(hashedMessage)

	decodedMessage := hexutil.MustDecode(signedMessage)

	if decodedMessage[64] == 27 || decodedMessage[64] == 28 {
		decodedMessage[64] -= 27
	}

	sigPublicKeyECDSA, err := crypto.SigToPub(hash.Bytes(), decodedMessage)
	if sigPublicKeyECDSA == nil {
		err = fmt.Errorf("could not get a public get from the message signature")
	}
	if err != nil {
		return false, err
	}

	return pubKStr == crypto.PubkeyToAddress(*sigPublicKeyECDSA).String(), nil
}

func convertGpuName(name string) string {
	if strings.TrimSpace(name) == "" {
		return ""
	} else {
		name = strings.Split(name, ":")[0]
	}
	if strings.Contains(name, "NVIDIA") {
		if strings.Contains(name, "Tesla") {
			return strings.Replace(name, "Tesla ", "", 1)
		}

		if strings.Contains(name, "GeForce") {
			name = strings.Replace(name, "GeForce ", "", 1)
		}
		return strings.Replace(name, "RTX ", "", 1)
	} else {
		if strings.Contains(name, "GeForce") {
			cpName := strings.Replace(name, "GeForce ", "NVIDIA", 1)
			return strings.Replace(cpName, "RTX", "", 1)
		}
	}
	return name
}
