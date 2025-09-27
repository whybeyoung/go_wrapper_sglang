package main

import (
	"archive/zip"
	"bufio"
	"bytes"
	"comwrapper"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"text/template"
	"time"
	"unsafe"

	"git.iflytek.com/AIaaS/xsf/utils"
	"github.com/whybeyoung/go-openai"
	gwsUtils "github.com/whybeyoung/go_wrapper_sglang/utils"
)

type PatchRes struct {
	InnerPatchId string
	PatchId      string
	LoraPath     string
}

var (
	wLogger                     *utils.Logger
	logLevel                    = "info"
	logCount                    = 10
	logSize                     = 30
	logAsync                    = true
	logPath                     = "/log/app/wrapper.log"
	respKey                     = "content"
	requestManager              *RequestManager
	httpServerPort              = 40000
	streamContextTimeoutSeconds = 5400 * time.Second
	// temperature                 = 0.95
	// maxTokens                   = 0
	// topP                        = 0.95
	promptSearchTemplate        string // 添加全局变量存储搜索模板
	promptSearchTemplateNoIndex string // 添加全局变量存储不带索引的搜索模板
	finetuneType                = ""   // 添加全局变量存储finetune类型
	// inferType                   string            // 添加全局变量存储推理类型
	patchResMap            map[string]PatchRes     // 添加全局变量存储InnerPatchId id path map
	patchIdInnerPatchIdMap map[string]string       // 添加全局变量存储patchId InnerPatchId map
	patchIdMapMutex        sync.RWMutex            // 添加锁保护patchIdMap
	pretrainedName         string                  // 添加全局变量存储pretrained name
	server                 = "http://127.0.0.1:%d" // 添加全局变量存储server url
	isReasoningModel       bool
	cmd                    *exec.Cmd
	globalEnableThinking          = true
	loadLoraAdapter               = "load_lora_adapter"
	unloadLoraAdapter             = "unload_lora_adapter"
	reasoningEffort               = "high"
	enableAppIdHeader      bool   = true // 是否启用 appID 请求级别metadata
	customerHeader         string = "x-customer-labels"
	maxStopWords           int    = 16 // 添加全局变量存储最大停止词数
)

var traceFunc func(usrTag string, key string, value string) (code int)
var meterFunc func(usrTag string, key string, count int) (code int)
var LbExtraFunc func(params map[string]string) error

const (
	R1_THINK_START                  = "<think>"
	R1_THINK_END                    = "</think>"
	GPT_THINK_START                 = "analysis"
	GPT_THINK_END_ASSISTANT         = "assistant"
	GPT_THINK_END_FINAL             = "final"
	HTTP_SERVER_REQUEST_TIMEOUT     = time.Second * 10
	HTTP_SERVER_MAX_RETRY_TIME      = time.Minute * 60
	STREAM_CONNECT_TIMEOUT          = "stream connect timeout"
	ENGINE_TOKEN_LIMIT_EXCEEDED_ERR = "ENGINE_TOKEN_LIMIT_EXCEEDED;code=2001"
	ENGINE_REQUEST_ERR              = "ENGINE_REQUEST_ERR;code=2002"

	DEFAULT_MODEL_NAME = "default"
	LORA_WEIGHT_PATH   = "/home/lora_weight"
)

// RequestManager 请求管理器
type RequestManager struct {
	Client      *OpenAIClient
	Logger      *utils.Logger
	RequestLock sync.RWMutex
}

// NewRequestManager 创建请求管理器
func NewRequestManager(client *OpenAIClient) *RequestManager {
	logger, err := utils.NewLocalLog(
		utils.SetAsync(logAsync),
		utils.SetLevel(logLevel),
		utils.SetFileName(logPath),
		utils.SetMaxSize(logSize),
		utils.SetMaxBackups(logCount),
	)
	if err != nil {
		panic(fmt.Sprintf("Failed to create logger: %v", err))
	}

	return &RequestManager{
		Client: client,
		Logger: logger,
	}
}
func getFreePort() (int, error) {
	// 监听一个系统分配端口（0 表示让系统自动选择）
	listener, err := net.Listen("tcp", ":0")
	if err != nil {
		return 0, err
	}
	defer listener.Close()

	// 获取监听的实际地址
	addr := listener.Addr().(*net.TCPAddr)
	return addr.Port, nil
}

func writePortToFile(port int) error {
	file, err := os.Create("/home/aiges/sglangport")
	if err != nil {
		return err
	}
	defer file.Close()
	_, err = fmt.Fprintf(file, "%d", httpServerPort)
	return err
}

func getEnvValue(key string) string {
	envStr := os.Getenv(key)
	wLogger.Infof("getEnvValue %s=%s", key, envStr)
	return envStr
}

// WrapperInit 插件初始化, 全局只调用一次. 本地调试时, cfg参数由aiges.toml提供
func WrapperInit(cfg map[string]string) (err error) {
	fmt.Printf("---- wrapper init ----\n")
	for k, v := range cfg {
		fmt.Printf("config param %s=%s\n", k, v)
	}
	if v, ok := cfg["log_path"]; ok {
		logPath = v
	}
	if v, ok := cfg["log_level"]; ok {
		logLevel = v
	}
	if v, ok := cfg["log_count"]; ok {
		logCount, _ = strconv.Atoi(v)
	}
	if v, ok := cfg["log_size"]; ok {
		logSize, _ = strconv.Atoi(v)
	}
	if cfg["log_async"] == "false" {
		logAsync = false
	}
	if v, ok := cfg["resp_key"]; ok {
		respKey = v
	}
	// 获取搜索模板
	if v, ok := cfg["prompt_search_template"]; ok {
		promptSearchTemplate = v
	}
	// 获取不带索引的搜索模板
	if v, ok := cfg["prompt_search_template_no_index"]; ok {
		promptSearchTemplateNoIndex = v
	}
	if v, ok := cfg["stream_context_timeout_seconds"]; ok {
		if t, err := strconv.ParseInt(v, 10, 64); err == nil {
			streamContextTimeoutSeconds = time.Second * time.Duration(t)
		}
	}
	if v, ok := cfg["max_stop_words"]; ok {
		maxStopWords, _ = strconv.Atoi(v)
	}

	// 创建日志目录
	logDir := filepath.Dir(logPath)
	if err := os.MkdirAll(logDir, 0755); err != nil {
		return fmt.Errorf("failed to create log directory: %v", err)
	}

	wLogger, err = utils.NewLocalLog(
		utils.SetAsync(logAsync),
		utils.SetLevel(logLevel),
		utils.SetFileName(logPath),
		utils.SetMaxSize(logSize),
		utils.SetMaxBackups(logCount),
	)
	if err != nil {
		return fmt.Errorf("loggerErr:%v", err)
	}
	wLogger.Infof("wrapper config:%v", cfg)

	// 获取搜索模板
	if promptSearchTemplate != "" {
		wLogger.Infow("Using custom prompt search template")
	} else {
		wLogger.Infow("No prompt search template provided")
	}

	// 从环境变量获取配置
	baseModel := getEnvValue("FULL_MODEL_PATH")

	isReasoningModelStr := getEnvValue("IS_REASONING_MODEL")
	if isReasoningModelStr == "true" {
		isReasoningModel = true
	}

	reasoningEffortStr := getEnvValue("REASONING_EFFORT")
	if reasoningEffortStr != "" {
		reasoningEffort = reasoningEffortStr
	}

	// 获取端口配置
	port := getEnvValue("HTTP_SERVER_PORT")
	if port != "" {
		httpServerPort, _ = strconv.Atoi(port)
		if httpServerPort == 0 {
			httpServerPort, err = getFreePort()
			if err != nil {
				return fmt.Errorf("getFreePort:%v", err)
			}
		}
	}

	err = writePortToFile(httpServerPort)
	if err != nil {
		return fmt.Errorf("cant write port to file:%v", err)
	}

	// 获取额外参数
	extraArgs := getEnvValue("CMD_EXTRA_ARGS")
	if extraArgs == "" {
		extraArgs = "--mem-fraction-static 0.93 --torch-compile-max-bs 8 --max-running-requests 20"
		wLogger.Infow("Using default CMD_ARGS", "args", extraArgs)
	} else {
		wLogger.Infow("Using custom CMD_ARGS", "args", extraArgs)
	}

	// 检查是否启用指标
	enableMetrics := getEnvValue("ENABLE_METRICS")
	if enableMetrics == "true" {
		extraArgs += " --enable-metrics"
		wLogger.Infow("Metrics enabled")
	}
	// 读取generation_config.json文件
	preferredSamplingParams := getEnvValue("PREFERRED_SAMPLING_PARAMS")
	if preferredSamplingParams != "" {
		extraArgs += fmt.Sprintf(" --preferred-sampling-params %v ", preferredSamplingParams)
	} else if baseModel != "" {
		genConfig, err := readGenerationConfig(baseModel)
		if err != nil {
			wLogger.Warnw("Failed to read generation config", "error", err)
		} else {
			if len(genConfig) == 0 {
				wLogger.Infow("Generation config is empty")
			} else {
				// 将配置信息转换为JSON并打印
				configBytes, _ := json.Marshal(genConfig)
				configStr := string(configBytes)
				wLogger.Infow("Generation config loaded successfully",
					"configStr", configStr)
				extraArgs += fmt.Sprintf(" --preferred-sampling-params %v ", configStr)
			}
		}
	}

	// 检查是否启用tools
	toolCallParser := getEnvValue("TOOL_CALL_PARSER")
	if len(toolCallParser) > 0 {
		extraArgs += fmt.Sprintf(" --tool-call-parser %v ", toolCallParser)
	}
	// 检查多节点模式
	enableMultiNode := getEnvValue("ENABLE_MULTI_NODE_MODE")
	if enableMultiNode == "true" {
		wLogger.Infow("Multi-node mode enabled")

		// 获取组大小
		if groupSize := getEnvValue("LWS_GROUP_SIZE"); groupSize != "" {
			extraArgs += fmt.Sprintf(" --nnodes %s", groupSize)
		}

		// 获取leader地址
		if leaderAddr := getEnvValue("LWS_LEADER_ADDRESS"); leaderAddr != "" {

			distPort := getEnvValue("DIST_PORT")
			if distPort == "" {
				distPort = "20000"
			}
			extraArgs += fmt.Sprintf(" --dist-init-addr %s:%s", leaderAddr, distPort)
		}

		// 获取worker索引
		if workerIndex := getEnvValue("LWS_WORKER_INDEX"); workerIndex != "" {
			extraArgs += fmt.Sprintf(" --node-rank %s", workerIndex)
		}
	}

	pretrainedName = getEnvValue("PRETRAINED_MODEL_NAME")
	finetuneType = getEnvValue("FINETUNE_TYPE")
	loraSetting := getEnvValue("LORA_SETTING")
	if loraSetting != "" {
		if finetuneType == "" {
			finetuneType = "lora"
		}
		extraArgs += loraSetting
	}
	globalEnableThinkingStr := getEnvValue("GLOBAL_ENABLE_THINKING")
	if globalEnableThinkingStr == "false" {
		globalEnableThinking = false
	}

	// 构建完整的启动命令
	args := []string{
		"-m", "sglang.launch_server",
		"--model-path", baseModel,
		"--port", strconv.Itoa(httpServerPort),
		"--trust-remote",
		"--host", "0.0.0.0",
		"--enable-metrics",
	}
	// 添加额外参数
	args = append(args, strings.Fields(extraArgs)...)

	// 启动sglang服务
	cmd = exec.Command("python", args...)
	wLogger.Infow("Starting sglang server", "command", strings.Join(cmd.Args, " "))
	fmt.Printf("Starting sglang server \n")
	// 创建管道捕获输出
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("failed to create stdout pipe: %v", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return fmt.Errorf("failed to create stderr pipe: %v", err)
	}

	// 启动服务进程 - Start()是非阻塞的
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("failed to start sglang server: %v", err)
	}

	// 启动输出监控协程
	go func() {
		scanner := bufio.NewScanner(stdout)
		for scanner.Scan() {
			fmt.Println(scanner.Text())
		}
	}()

	go func() {
		scanner := bufio.NewScanner(stderr)
		for scanner.Scan() {
			fmt.Println(scanner.Text())
		}
	}()

	// 启动监控协程
	go monitorSubprocess(cmd)

	server = fmt.Sprintf(server, httpServerPort)
	// 等待服务就绪
	if err := waitServerReady(fmt.Sprintf("%s/health", server)); err != nil {
		return fmt.Errorf("server failed to start: %v", err)
	}

	// sglang指标上报
	//go reportSglangMetrics(fmt.Sprintf("http://localhost:%d/metrics", httpServerPort))

	// 初始化OpenAI客户端
	serverURL := fmt.Sprintf("%s/v1", server)
	client := NewOpenAIClient(serverURL)

	// 初始化请求管理器并保存到全局变量
	requestManager = NewRequestManager(client)

	// 检查是否启用预热
	if err := WarmupRequest(fmt.Sprintf("http://localhost:%d/v1/chat/completions", httpServerPort), "/home/aiges/warmup-data.json"); err != nil {
		wLogger.Errorw("Warmup failed", "error", err)
		return err
	} else {
		wLogger.Infow("Warmup completed successfully")
	}
	// 初始化patchIdMap
	patchResMap = make(map[string]PatchRes)
	patchIdInnerPatchIdMap = make(map[string]string)

	err = gwsUtils.UpdatePodMetricsPort(httpServerPort)
	if err != nil {
		wLogger.Errorw("updatePodMetricsPort failed", "error", err)
		return err
	}
	// 获取是否启用 appID 请求级别metadata
	enableAppIdHeaderStr := os.Getenv("ENABLE_APP_ID_HEADER")
	if enableAppIdHeaderStr == "false" {
		enableAppIdHeader = false
		wLogger.Infow("Enable App Id Header is disabled")
	} else {
		wLogger.Infow("Enable App Id Header is enabled")
	}
	wLogger.Debugw("WrapperInit successful")
	return
}

func updatePodMetricsPort() {

}

func reportSglangMetrics(serverMetricURL string) {
	wLogger.Debugw("sglang metric start")
	httpClient := http.Client{
		Timeout: time.Second,
	}
	sleepInterval := time.Second
	for {

		resp, err := httpClient.Get(serverMetricURL)
		if err != nil {
			wLogger.Errorw("sglang metric", "err", err.Error())
			time.Sleep(sleepInterval)
			continue
		}
		body, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil {
			wLogger.Errorw("sglang metric", "err", err.Error())
			time.Sleep(sleepInterval)
			continue
		}
		wLogger.Debugw("sglang metric", "resp", string(body))
		sglangMetrics := gwsUtils.ParseMetrics(string(body))
		res := make(map[string]string, len(sglangMetrics))
		for k, v := range sglangMetrics {
			str, _ := json.Marshal(v)
			res[k] = string(str)
		}
		err = LbExtraFunc(res)
		if err != nil {
			wLogger.Errorw("sglang metric", "err", err.Error())
		}
		time.Sleep(sleepInterval)
	}
}

// waitServerReady 等待服务器就绪
func waitServerReady(serverURL string) error {
	httpClient := http.Client{
		Timeout: HTTP_SERVER_REQUEST_TIMEOUT,
	}
	retries := 0
	timeBegin := time.Now()
	for {
		resp, err := httpClient.Get(serverURL)
		wLogger.Debugw("Server resp", "resp", resp, "err", err)
		if err == nil && resp.StatusCode == http.StatusOK {
			wLogger.Debugw("Server ready", "url", serverURL)
			return nil
		}

		if resp != nil {
			resp.Body.Close()
		}

		retries++
		timeInerval := time.Since(timeBegin)
		if timeInerval >= HTTP_SERVER_MAX_RETRY_TIME {
			break
		}

		wLogger.Debugw("Server not ready, retrying...", "url", serverURL, "attempt", retries)
		time.Sleep(5 * time.Second)
	}
	return fmt.Errorf("server failed to start after %d attempts", retries)
}

// getLocalIP 获取本地IP
func getLocalIP() string {
	conn, err := net.Dial("udp", "8.8.8.8:80")
	if err != nil {
		return "127.0.0.1"
	}
	defer conn.Close()

	localAddr := conn.LocalAddr().(*net.UDPAddr)
	return localAddr.IP.String()
}

// wrapperInst 结构体定义
type wrapperInst struct {
	usrTag               string
	sid                  string
	appId                string
	client               *OpenAIClient
	stopQ                chan bool
	firstFrame           bool
	callback             comwrapper.CallBackPtr
	params               map[string]string
	active               bool
	continueFinalMessage bool
}

// WrapperCreate 插件会话实例创建
func WrapperCreate(usrTag string, params map[string]string, prsIds []int, cb comwrapper.CallBackPtr) (hdl unsafe.Pointer, err error) {
	sid := params["sid"]
	paramStr := ""
	for k, v := range params {
		paramStr += fmt.Sprintf("%s=%s;", k, v)
	}
	wLogger.Debugw("WrapperCreate start", "paramStr", paramStr, "sid", sid)
	appId := params["app_id"]

	// 创建新的实例
	inst := &wrapperInst{
		usrTag:     usrTag,
		sid:        sid,
		appId:      appId,
		client:     requestManager.Client,
		stopQ:      make(chan bool, 2),
		firstFrame: true,
		callback:   cb,
		params:     params,
		active:     true, // 初始化active为true
	}

	wLogger.Infow("WrapperCreate successful", "sid", sid, "usrTag", usrTag)
	return unsafe.Pointer(inst), nil
}

func responseContent(status comwrapper.DataStatus, index int, text string, resoning_content string, function_call []openai.FunctionCall) (comwrapper.WrapperData, error) {

	choice := map[string]interface{}{
		"content":           text,
		"reasoning_content": resoning_content,
		"index":             0,
		"role":              "assistant",
	}
	if len(function_call) > 0 {
		choice["function_call"] = function_call
	}
	result := map[string]interface{}{
		"choices": []map[string]interface{}{
			choice,
		},
		"question_type": "",
	}
	data, err := json.Marshal(result)

	return comwrapper.WrapperData{
		Key:      "content",
		Data:     data,
		Desc:     nil,
		Encoding: "utf-8",
		Type:     comwrapper.DataText,
		Status:   status,
	}, err
}

func responseUsage(status comwrapper.DataStatus, prompt_token, completion_token int) (comwrapper.WrapperData, error) {
	result := map[string]interface{}{
		"prompt_tokens":     prompt_token,
		"completion_tokens": completion_token,
		"total_tokens":      prompt_token + completion_token,
		"question_tokens":   4, // 无意义，未使用默认值4
	}
	data, err := json.Marshal(result)

	return comwrapper.WrapperData{
		Key:      "usage",
		Data:     data,
		Desc:     nil,
		Encoding: "utf-8",
		Type:     comwrapper.DataText,
		Status:   status,
	}, err
}

func responseError(inst *wrapperInst, err error) {
	wLogger.Errorf("WrapperWrite stream err:%v, sid:%v\n", err.Error(), inst.sid)

	if err = inst.callback(inst.usrTag, nil, err); err != nil {
		wLogger.Errorw("WrapperWrite error callback failed", "error", err, "sid", inst.sid)
	}
}

func responseEnd(inst *wrapperInst, index int, prompt_tokens_len, result_tokens_len int) error {
	status := comwrapper.DataEnd
	usageWrapperData, err := responseUsage(status, prompt_tokens_len, result_tokens_len)
	if err != nil {
		responseError(inst, err)
		return err
	}
	content, err := responseContent(status, index, "", "", nil)
	if err != nil {
		responseError(inst, err)
		return err
	}
	responseData := []comwrapper.WrapperData{content, usageWrapperData}
	wLogger.Infof("WrapperWrite stream responseEnd index:%v, prompt_tokens_len:%v, result_tokens_len:%v,responseData:%v, sid:%v\n", index, prompt_tokens_len, result_tokens_len, responseData, inst.sid)
	if err = inst.callback(inst.usrTag, responseData, nil); err != nil {
		wLogger.Errorw("WrapperWrite end callback error", "error", err, "sid", inst.sid)
		return err
	}
	return nil
}

func toString(v any) string {
	if v == nil {
		return "nil"
	}
	res, err := json.Marshal(v)
	if err != nil {
		return fmt.Sprintf("{%T: %v}", v, v)
	}
	return string(res)
}

// schemaMarshaler 自定义的 Marshaler 类型
type schemaMarshaler struct {
	data []byte
}

func (s schemaMarshaler) MarshalJSON() ([]byte, error) {
	return s.data, nil
}

// 额外参数
type ExtraParams struct {
	ResponseFormat *struct {
		Type       string `json:"type,omitempty"`
		JSONSchema *struct {
			Name        string                 `json:"name"`
			Description string                 `json:"description,omitempty"`
			Schema      map[string]interface{} `json:"schema"`
			Strict      bool                   `json:"strict"`
		} `json:"json_schema,omitempty"`
	} `json:"response_format,omitempty"`
	LogitBias            map[string]int `json:"logit_bias,omitempty"`
	ReasoningEffort      string         `json:"reasoning_effort,omitempty"`
	FrequencyPenalty     *float32       `json:"frequency_penalty,omitempty"`
	PresencePenalty      *float32       `json:"presence_penalty,omitempty"`
	ContinueFinalMessage bool           `json:"continue_final_message,omitempty"`
	Stop                 []string       `json:"stop,omitempty"`
}

// app_Id
type CustomerLabel struct {
	AppId string `json:"app_id,omitempty"`
}

func openaiFunctionCall(inst *wrapperInst, functions []openai.FunctionDefinition, req *openai.ChatCompletionRequest) error {
	openaiTools := make([]openai.Tool, len(functions))
	for i := range functions {
		openaiTools[i] = openai.Tool{
			Type:     "function",
			Function: &functions[i],
		}
	}
	req.Tools = openaiTools
	req.ToolChoice = "auto"
	req.Stream = false
	// req := openai.ChatCompletionRequest{
	// 	Model:      "default",
	// 	Messages:   convertToOpenAIMessages(openaiMsgs),
	// 	Tools:      openaiTools,
	// 	ToolChoice: "auto",
	// 	Stream:     false,
	// }
	wLogger.Debugf("WrapperWrite function_call req:%v, sid:%v\n", toString(req), inst.sid)
	ctx := context.Background()
	resp, err := inst.client.openaiClient.CreateChatCompletion(ctx, *req)
	if err != nil {
		wLogger.Errorw("WrapperWrite function_call CreateChatCompletion error", "error", err, "sid", inst.sid)
		responseError(inst, err)
		return err
	}
	var toolCalls []openai.FunctionCall
	if len(resp.Choices) > 0 && len(resp.Choices[0].Message.ToolCalls) > 0 {
		for _, v := range resp.Choices[0].Message.ToolCalls {
			toolCalls = append(toolCalls, openai.FunctionCall{
				Name:      v.Function.Name,
				Arguments: v.Function.Arguments,
			})
		}
	}
	wLogger.Debugf("WrapperWrite function_call toolcalls:%v, sid:%v\n response:%v", toString(toolCalls), inst.sid, toString(resp))
	// 整理结果
	status := comwrapper.DataEnd
	openaiContent := resp.Choices[0].Message.Content
	if openaiContent == "" {
		openaiContent = " "
	}
	content, err := responseContent(status, 0, openaiContent, "", toolCalls)
	if err != nil {
		wLogger.Errorw("WrapperWrite error function_calls callback", "error", err, "sid", inst.sid)
		responseError(inst, err)
		return err
	}

	usage, err := responseUsage(status, resp.Usage.PromptTokens, resp.Usage.CompletionTokens)
	if err != nil {
		wLogger.Errorw("WrapperWrite error function_calls callback", "error", err, "sid", inst.sid)
		responseError(inst, err)
		return err
	}
	if err = inst.callback(inst.usrTag, []comwrapper.WrapperData{content, usage}, nil); err != nil {
		wLogger.Errorw("WrapperWrite error function_calls callback failed", "error", err, "sid", inst.sid)
		return err
	}
	return nil
}
func buildStreamReq(inst *wrapperInst, req comwrapper.WrapperData) (*openai.ChatCompletionRequest, []openai.FunctionDefinition, bool) {
	// 从params中获取参数, 否则不传
	var (
		temp = float64(0)
		mt   = 0
		tp   = float64(0)
		stop []string
	)
	if tempStr, ok := inst.params["temperature"]; ok {
		if t, err := strconv.ParseFloat(tempStr, 64); err == nil {
			temp = t
		} else {
			wLogger.Warnw("Invalid temperature value", "value", tempStr, "sid", inst.sid)
		}
	}

	if tokens, ok := inst.params["max_tokens"]; ok {
		if t, err := strconv.Atoi(tokens); err == nil {
			mt = t
		} else {
			wLogger.Warnw("Invalid max_tokens value", "value", tokens, "sid", inst.sid)
		}
	}

	if tpStr, ok := inst.params["top_p"]; ok {
		if t, err := strconv.ParseFloat(tpStr, 32); err == nil {
			tp = t
		} else {
			wLogger.Warnw("Invalid top_p value", "value", tpStr, "sid", inst.sid)
		}
	}

	streamReq := &openai.ChatCompletionRequest{
		Model: DEFAULT_MODEL_NAME,
	}
	wLogger.Infow("WrapperWrite request parameters",
		"sid", inst.sid,
		"temperature", temp,
		"maxTokens", mt,
		"topP", tp)
	if mt > 0 {
		streamReq.MaxTokens = mt
	}
	if temp > 0 {
		streamReq.Temperature = float32(temp)
	}
	if tp > 0 {
		streamReq.TopP = float32(tp)
	}
	streamReq.Stream = true
	streamReq.StreamOptions = &openai.StreamOptions{
		IncludeUsage: true,
	}
	// 设定 appID 请求级别，用于统计
	if inst.appId != "" && enableAppIdHeader {
		appIdHeader := CustomerLabel{
			AppId: inst.appId,
		}
		appIdHeaderStr, err := json.Marshal(appIdHeader)
		if err != nil {
			wLogger.Errorw("WrapperWrite marshal appIdHeader error", "error", err, "sid", inst.sid, "appId", inst.appId)
		}
		streamReq.Metadata = map[string]string{
			customerHeader: string(appIdHeaderStr),
		}
	}

	enableThinking := globalEnableThinking
	if enable_thinking, ok := inst.params["enable_thinking"]; ok {
		if enable_thinking == "false" {
			enableThinking = false
		} else {
			enableThinking = true
		}
	}
	streamReq.ExtraBody = map[string]any{
		"chat_template_kwargs": map[string]interface{}{
			"enable_thinking": enableThinking,
			"thinking":        enableThinking, // deepseekv31使用
		},
	}

	lora_path := ""
	if finetuneType == "lora" || finetuneType == "qlora" {
		if patch_id, ok := inst.params["patch_id"]; ok && patch_id != "" {
			patchIdMapMutex.RLock()
			if lora_path, ok = patchIdInnerPatchIdMap[patch_id]; ok {
				lora_path = patch_id
			} else {
				wLogger.Errorw("lora or qlora infer, but lora path is invalid ", "patch_id", patch_id, "sid", inst.sid)
			}
			patchIdMapMutex.RUnlock()

		}
	}
	if lora_path != "" {
		// enable_thinking 为true有效果
		streamReq.ExtraBody["lora_path"] = lora_path
	}
	// 从params中获取 extra_body
	extra_parms := ""
	if v, ok := inst.params["extra_body"]; ok {
		extra_parms = v
	}
	// 解析 extra_parms
	var extraParams ExtraParams
	if err := json.Unmarshal([]byte(extra_parms), &extraParams); err != nil {
		wLogger.Errorw("WrapperWrite unmarshal extra_parms error", "error", err, "sid", inst.sid, "extra_parms", extra_parms)
	}
	if extraParams.ReasoningEffort != "" {
		streamReq.ExtraBody["reasoning_effort"] = extraParams.ReasoningEffort
	} else {
		streamReq.ExtraBody["reasoning_effort"] = reasoningEffort
	}
	// 解析 extra_parms 的 response_format string 反序列化 ChatCompletionResponseFormat格式
	var responseFormat openai.ChatCompletionResponseFormat
	if extraParams.ResponseFormat != nil {
		responseFormat.Type = openai.ChatCompletionResponseFormatType(extraParams.ResponseFormat.Type)
		if extraParams.ResponseFormat.JSONSchema != nil {
			// 将 schema 转换为 json.RawMessage
			schemaBytes, err := json.Marshal(extraParams.ResponseFormat.JSONSchema.Schema)
			if err != nil {
				wLogger.Errorw("WrapperWrite marshal schema error", "error", err, "sid", inst.sid)
			} else {
				// 创建一个自定义的 Marshaler 类型
				responseFormat.JSONSchema = &openai.ChatCompletionResponseFormatJSONSchema{
					Name:        extraParams.ResponseFormat.JSONSchema.Name,
					Description: extraParams.ResponseFormat.JSONSchema.Description,
					Schema:      schemaMarshaler{data: schemaBytes},
					Strict:      extraParams.ResponseFormat.JSONSchema.Strict,
				}
			}
		}
	}
	if responseFormat.Type != "" {
		streamReq.ResponseFormat = &responseFormat
	}
	if extraParams.FrequencyPenalty != nil {
		streamReq.FrequencyPenalty = *extraParams.FrequencyPenalty
	}
	if extraParams.PresencePenalty != nil {
		streamReq.PresencePenalty = *extraParams.PresencePenalty
	}
	if extraParams.ContinueFinalMessage {
		inst.continueFinalMessage = true
	}
	if len(extraParams.Stop) > 0 {
		if len(extraParams.Stop) > maxStopWords {
			wLogger.Warnw("WrapperWrite stop words over limit", "stop", extraParams.Stop, "maxStopWords", maxStopWords, "sid", inst.sid)
			stop = extraParams.Stop[:maxStopWords]
		} else {
			stop = extraParams.Stop
			wLogger.Warnw("WrapperWrite stop words", "stop", extraParams.Stop, "sid", inst.sid)

		}

	}

	openaiMsgs, functions := inst.formatMessages(string(req.Data), promptSearchTemplate, promptSearchTemplateNoIndex)
	thinking := false
	if isReasoningModel {
		lastMsg := openaiMsgs[len(openaiMsgs)-1]
		if lastMsg.Role != "assistant" {
			openaiMsgs = append(openaiMsgs, Message{
				Role:    "assistant",
				Content: R1_THINK_START + "\n",
			})
			thinking = true
		}
	}
	if inst.continueFinalMessage {
		streamReq.ExtraBody["continue_final_message"] = true
	}
	if len(stop) > 0 {
		streamReq.Stop = stop
	}
	streamReq.Messages = convertToOpenAIMessages(openaiMsgs)
	return streamReq, functions, thinking
}

// WrapperWrite 数据写入
func WrapperWrite(hdl unsafe.Pointer, req []comwrapper.WrapperData) (err error) {
	inst := (*wrapperInst)(hdl)
	wLogger.Infof("WrapperWrite inst:%v, req:%v\n", toString(inst), toString(req))
	if !inst.active {
		wLogger.Warnw("WrapperWrite called on inactive instance", "sid", inst.sid)
		return fmt.Errorf("instance is not active")
	}

	if len(req) == 0 {
		wLogger.Debugw("WrapperWrite data is nil", "sid", inst.sid)
		return nil
	}

	wLogger.Infow("WrapperWrite start", "sid", inst.sid, "usrTag", inst.usrTag)

	// 检查是否已停止
	select {
	case stopped := <-inst.stopQ:
		if stopped {
			wLogger.Infow("WrapperWrite stopped by signal", "sid", inst.sid)
			return nil
		}
	default:
	}

	for _, v := range req {
		if v.Key == "__kv_info" {
			continue // 跳过kv_info数据
		}

		wLogger.Infow("WrapperWrite processing data", "data", string(v.Data), "status", v.Status, "sid", inst.sid)

		streamReq, functions, thinking := buildStreamReq(inst, v)

		// 如果是fuction请求
		if len(functions) > 0 {
			return openaiFunctionCall(inst, functions, streamReq)
		}
		promptTokensLen, resultTokensLen := 0, 0

		// 创建流式请求
		// streamReq := &openai.ChatCompletionRequest{
		// 	Model:       "default",
		// 	Messages:    convertToOpenAIMessages(openaiMsgs),
		// 	Temperature: float32(temperature),
		// 	MaxTokens:   maxTokens,
		// 	Stream:      true,
		// 	StreamOptions: &openai.StreamOptions{
		// 		IncludeUsage: true,
		// 	},
		// 	TopP: float32(topP),
		// }
		wLogger.Infof("WrapperWrite streamReq:%v, %v\n", toString(streamReq), inst.sid)

		// 使用协程处理流式请求
		go func(req *openai.ChatCompletionRequest, status comwrapper.DataStatus) {
			defer func() {
				if r := recover(); r != nil {
					wLogger.Errorw("WrapperWrite panic: ", "sid", inst.sid, "r", r)
				}
			}()
			wLogger.Infow("WrapperWrite starting stream inference", "sid", inst.sid)

			ctx, cancel := context.WithTimeout(context.Background(), streamContextTimeoutSeconds)
			defer cancel()

			stream, err := inst.client.openaiClient.CreateChatCompletionStream(ctx, *req)
			if err != nil {
				wLogger.Errorw("WrapperWrite stream error", "error", err, "sid", inst.sid)
				responseError(inst, err)
				return
			}
			defer stream.Close()

			index := 0
			fullContent := ""
			// pre_chunk_content := ""

			status = comwrapper.DataContinue
			// 首帧返回空
			firstFrameContent, err := responseContent(status, index, "", "", nil)
			if err != nil {
				wLogger.Errorw("WrapperWrite error fisrt callback", "error", err, "sid", inst.sid)
				responseError(inst, err)
				return
			}
			wLogger.Infof("fisrtFrameContent:%v index:%v,status:%v sid:%v\n", "", index, status, inst.sid)
			if err := inst.callback(inst.usrTag, []comwrapper.WrapperData{firstFrameContent}, nil); err != nil {
				wLogger.Errorw("WrapperWrite error callback failed", "error", err, "sid", inst.sid)
				return
			}
			index += 1
			//status = comwrapper.DataContinue
			for {
				select {
				case <-ctx.Done():
					if ctx.Err() == context.DeadlineExceeded {
						wLogger.Warnw("WrapperWrite stream timeout", "sid", inst.sid)
						responseError(inst, fmt.Errorf(STREAM_CONNECT_TIMEOUT))
						goto endLoop
					}
					return
				default:
					// 创建响应数据切片
					responseData := make([]comwrapper.WrapperData, 0, 2) // 预分配容量为2

					response, err := stream.Recv()
					if err != nil {
						if err == io.EOF {
							wLogger.Infow("WrapperWrite stream ended normally", "sid", inst.sid)
							goto endLoop
						}
						// 检查是否是token超限错误
						if isTokenLimitExceededError(err) {
							wLogger.Warnw("Token limit exceeded error detected", "error", err, "sid", inst.sid)
							// 创建一个自定义的token超限错误
							err = errors.New(ENGINE_TOKEN_LIMIT_EXCEEDED_ERR)
						} else {
							wLogger.Errorw("WrapperWrite read stream error", "error", err, "sid", inst.sid)
						}
						responseError(inst, err)
						return
					}
					if index == 1 {
						wLogger.Infow("WrapperWrite content first frame content", "response", toString(response), "sid", inst.sid)
					}
					if len(response.Choices) > 0 && response.Choices[0].Delta.ReasoningContent != "" {
						chunk_content := response.Choices[0].Delta.ReasoningContent
						wLogger.Debugf("WrapperWrite stream response reasoning index:%v, status:%v, chunk_content:%v, sid:%v\n", index, status, chunk_content, inst.sid)
						content, err := responseContent(status, index, "", chunk_content, nil)
						if err != nil {
							wLogger.Errorw("WrapperWrite content error", "error", err, "sid", inst.sid)
							responseError(inst, err)
							return
						}
						if err = inst.callback(inst.usrTag, []comwrapper.WrapperData{content}, nil); err != nil {
							wLogger.Errorw("WrapperWrite reasoning callback error", "error", err, "sid", inst.sid)
							return
						}
						index += 1
					} else if len(response.Choices) > 0 && response.Choices[0].Delta.Content != "" {
						var content comwrapper.WrapperData
						chunk_content := response.Choices[0].Delta.Content
						if chunk_content == R1_THINK_START {
							thinking = true
							continue
						}
						if strings.Contains(chunk_content, R1_THINK_END) {
							// content的内容是</think>，有可能出来2帧
							thinking = false
							chunks := strings.Split(chunk_content, R1_THINK_END)
							reasoning_last_chunk := chunks[0]
							answer_start_chunk := chunks[1]

							reasoning_last_chunk_content, err := responseContent(status, index, "", reasoning_last_chunk, nil)
							wLogger.Debugf("WrapperWrite stream response index:%v, status:%v, reasoning_last_chunk:%v, sid:%v\n", index, status, reasoning_last_chunk, inst.sid)
							if err != nil {
								wLogger.Errorw("WrapperWrite reasoning_last_chunk error", "error", err, "sid", inst.sid)
								responseError(inst, err)
								return
							}
							fullContent += reasoning_last_chunk
							responseData = append(responseData, reasoning_last_chunk_content)
							if err = inst.callback(inst.usrTag, responseData, nil); err != nil {
								wLogger.Errorw("WrapperWrite reasoning_last_chunk_content callback error", "error", err, "sid", inst.sid)
								return
							}
							index += 1

							answer_start_chunk_content, err := responseContent(status, index, answer_start_chunk, "", nil)
							if err != nil {
								wLogger.Errorw("WrapperWrite answer_start_chunk error", "error", err, "sid", inst.sid)
								responseError(inst, err)
								return
							}
							wLogger.Debugf("WrapperWrite stream response index:%v, status:%v, answer_start_chunk:%v, sid:%v\n", index, status, answer_start_chunk, inst.sid)
							responseData[0] = answer_start_chunk_content
							if err = inst.callback(inst.usrTag, responseData, nil); err != nil {
								wLogger.Errorw("WrapperWrite answer_start_chunk_content callback error", "error", err, "sid", inst.sid)
								return
							}
							index += 1
							continue
						}
						// if chunk_content == GPT_THINK_END_FINAL && pre_chunk_content == GPT_THINK_END_ASSISTANT && thinking {
						// 	thinking = false
						// 	continue
						// }
						// pre_chunk_content = chunk_content

						if thinking {
							content, err = responseContent(status, index, "", chunk_content, nil)
							if err != nil {
								wLogger.Errorw("WrapperWrite thinking content error", "error", err, "sid", inst.sid)
								responseError(inst, err)
								return
							}
						} else {
							content, err = responseContent(status, index, chunk_content, "", nil)
							if err != nil {
								wLogger.Errorw("WrapperWrite content error", "error", err, "sid", inst.sid)
								responseError(inst, err)
								return
							}
						}
						responseData = append(responseData, content)
						fullContent += chunk_content
						wLogger.Debugf("WrapperWrite stream response think:%v, index:%v, status:%v, content:%v, sid:%v\n", thinking, index, status, chunk_content, inst.sid)
						if err = inst.callback(inst.usrTag, responseData, nil); err != nil {
							wLogger.Errorw("WrapperWrite content callback error", "error", err, "sid", inst.sid)
							return
						}
						index += 1
					} else if response.Usage != nil {
						// 创建usage数据结构
						promptTokensLen = response.Usage.PromptTokens
						resultTokensLen = response.Usage.CompletionTokens
						wLogger.Debugf("WrapperWrite stream responseEnd index:%v, promptTokensLen:%v, resultTokensLen:%v sid:%v\n", index, promptTokensLen, resultTokensLen, inst.sid)
						err = responseEnd(inst, index, promptTokensLen, resultTokensLen)
						if err != nil {
							return
						}
					}

				}
			}

		endLoop:
			wLogger.Infow("WrapperWrite sending end signal", "sid", inst.sid, "fullContent", fullContent, "in_tokens", promptTokensLen, "out_tokens", resultTokensLen)
			inst.stopQ <- true

		}(streamReq, v.Status)

	}

	return nil
}

// isTokenLimitExceededError 检查是否是token超限错误
func isTokenLimitExceededError(err error) bool {
	if err == nil {
		return false
	}
	errorMsg := err.Error()
	// 检查是否包含token超限的关键字
	return strings.Contains(errorMsg, "Requested token count exceeds the model's maximum context length") ||
		strings.Contains(errorMsg, "maximum context length") ||
		strings.Contains(errorMsg, "is longer than the model's context length")
}

// WrapperDestroy 会话资源销毁
func WrapperDestroy(hdl interface{}) (err error) {
	inst := (*wrapperInst)(hdl.(unsafe.Pointer))
	wLogger.Infow("WrapperDestroy", "sid", inst.sid)
	inst.active = false
	inst.stopQ <- true

	return nil
}

func WrapperRead(hdl unsafe.Pointer) (respData []comwrapper.WrapperData, err error) {
	return
}

// WrapperFini 插件资源销毁
func WrapperFini() (err error) {
	fmt.Printf("WrapperFini called\n")
	fmt.Printf("WrapperFini cmd:%+v\n", toString(cmd))
	if cmd != nil && cmd.Process != nil {
		if err = cmd.Process.Kill(); err != nil {
			fmt.Printf("WrapperFini err:%v\n", err)
			wLogger.Infof("WrapperFini err:%v", err)
		} else {
			fmt.Printf("WrapperFini cmd kill success\n")
			wLogger.Infof("WrapperFini cmd kill success")
		}
	}
	wLogger.Infof("WarpperFini success")
	return
}

// WrapperVersion 获取版本信息
func WrapperVersion() (version string) {
	return
}

// WrapperLoadRes 加载资源
func WrapperLoadRes(res comwrapper.WrapperData, resId int) (err error) {
	wLogger.Debugw("WrapperLoadRes", "res", toString(res.Desc), res.Key)
	if res.Desc == nil {
		return fmt.Errorf("missing desc")
	}
	patch_id, ok := res.Desc["patch_id"]
	if !ok {
		return fmt.Errorf("missing patch_id in desc")
	}
	patchIdMapMutex.Lock()
	resIdStr := strconv.Itoa(resId)
	if _, ok := patchResMap[resIdStr]; ok {
		wLogger.Warnw("WrapperLoadRes resId already loaded", "resId", resId)
		patchIdMapMutex.Unlock()
		return nil
	}
	patchIdMapMutex.Unlock()

	loraWeightPath := filepath.Join(LORA_WEIGHT_PATH, patch_id)
	// 判断路径是否存在
	if _, err := os.Stat(loraWeightPath); err == nil {
		wLogger.Warnw("WrapperLoadRes path already exist", "resId", resId, "path", loraWeightPath)
	}

	// 创建buffer并解压zip文件
	buffer := bytes.NewBuffer(res.Data)
	if err := extractZipToPath(buffer, loraWeightPath); err != nil {
		wLogger.Errorw("WrapperLoadRes failed to extract zip", "resId", resId, "error", err)
		return fmt.Errorf("failed to extract zip: %v", err)
	}

	err = lora_load(pretrainedName, loraWeightPath, patch_id)
	if err != nil {
		wLogger.Errorw("WrapperLoadRes failed to load lora", "resId", resId, "error", err)
		return fmt.Errorf("failed to load lora: %v", err)
	}
	// 将路径存储到map中
	patchIdMapMutex.Lock()
	patchResMap[resIdStr] = PatchRes{
		InnerPatchId: resIdStr,
		PatchId:      patch_id,
		LoraPath:     loraWeightPath,
	}
	patchIdInnerPatchIdMap[patch_id] = resIdStr
	patchIdMapMutex.Unlock()
	wLogger.Infow("WrapperLoadRes success", "resId", resId, "path", loraWeightPath)
	return
}
func lora_load(pretrainedName, loraWeightPath, loraIdStr string) error {
	isValid, msg := checkValidLora(pretrainedName, loraWeightPath)
	if !isValid {
		wLogger.Errorw(msg)
		return fmt.Errorf(msg)
	}
	wLogger.Infof("checkValidLora:%v", msg)

	// lora 加载
	// TODO: 实现lora加载逻辑
	loadResp, err := PostRequest(fmt.Sprintf("%v/%v", server, loadLoraAdapter), map[string]interface{}{
		"lora_name": loraIdStr,
		"lora_path": loraWeightPath,
		// "base_model_name": pretrainedName,
	})
	if err != nil {
		wLogger.Errorw("WrapperLoadRes failed to load lora", "resId", loraIdStr, "error", err)
		return fmt.Errorf("failed to load lora: %v", err)
	}
	wLogger.Infof("WrapperLoadRes load lora success, resId:%v, loadResp:%v", loraIdStr, string(loadResp))

	return nil
}

// func lora_load_init() {
// 	// // 判断路径是否存在
// 	// if _, err := os.Stat(LORA_RECORD_PATH); err != nil {
// 	// 	wLogger.Errorw("lora_load path err", "path", LORA_RECORD_PATH, "err", err)
// 	// 	return
// 	// }
// 	if _, err := os.Stat(LORA_WEIGHT_PATH); err != nil {
// 		wLogger.Errorw("lora_load path err", "path", LORA_WEIGHT_PATH, "err", err)
// 		return
// 	}
// 	// recordfiles, err := os.ReadDir(LORA_RECORD_PATH)
// 	// if err != nil {
// 	// 	wLogger.Errorw("lora_load read dir err", "path", LORA_RECORD_PATH, "err", err)
// 	// 	return
// 	// }
// 	weightfiles, err := os.ReadDir(LORA_WEIGHT_PATH)
// 	if err != nil {
// 		wLogger.Errorw("lora_load read dir err", "path", LORA_WEIGHT_PATH, "err", err)
// 		return
// 	}
// 	weightMap := make(map[string]struct{}, len(weightfiles))
// 	for _, file := range weightfiles {
// 		weightPath := filepath.Join(LORA_WEIGHT_PATH, file.Name())
// 		err = lora_load(pretrainedName, weightPath, file.Name())
// 		if err != nil {
// 			wLogger.Errorw("lora_load failed to load lora", "path", weightPath, "error", err)
// 			continue
// 		}
// 		wLogger.Infow("lora_load load success", "path", weightPath)
// 		weightMap[file.Name()] = struct{}{}
// 	}
// }

// extractZipToPath 从内存中的zip数据解压到指定路径
func extractZipToPath(zipData *bytes.Buffer, extractPath string) error {
	// 创建zip reader
	zipReader, err := zip.NewReader(bytes.NewReader(zipData.Bytes()), int64(zipData.Len()))
	if err != nil {
		return fmt.Errorf("failed to create zip reader: %v", err)
	}

	// 确保目标目录存在
	if err := os.MkdirAll(extractPath, 0755); err != nil {
		return fmt.Errorf("failed to create extract directory: %v", err)
	}

	// 遍历zip文件中的所有文件
	for _, file := range zipReader.File {
		// 构建完整的文件路径
		filePath := filepath.Join(extractPath, file.Name)

		// 检查文件路径是否安全（防止zip slip攻击）
		if !strings.HasPrefix(filePath, filepath.Clean(extractPath)+string(os.PathSeparator)) {
			return fmt.Errorf("illegal file path: %s", file.Name)
		}

		// 如果是目录，创建目录
		if file.FileInfo().IsDir() {
			if err := os.MkdirAll(filePath, file.Mode()); err != nil {
				return fmt.Errorf("failed to create directory %s: %v", filePath, err)
			}
			continue
		}

		// 确保父目录存在
		if err := os.MkdirAll(filepath.Dir(filePath), 0755); err != nil {
			return fmt.Errorf("failed to create parent directory for %s: %v", filePath, err)
		}

		// 打开zip文件
		rc, err := file.Open()
		if err != nil {
			return fmt.Errorf("failed to open file %s in zip: %v", file.Name, err)
		}

		// 创建目标文件
		targetFile, err := os.OpenFile(filePath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, file.Mode())
		if err != nil {
			rc.Close()
			return fmt.Errorf("failed to create file %s: %v", filePath, err)
		}

		// 复制文件内容
		_, err = io.Copy(targetFile, rc)
		targetFile.Close()
		rc.Close()
		if err != nil {
			return fmt.Errorf("failed to write file %s: %v", filePath, err)
		}
	}

	return nil
}

func checkValidLora(pretrainedName string, loraPath string) (isValid bool, msg string) {
	configJsonPath := filepath.Join(loraPath, "adapter_config.json")
	if _, err := os.Stat(configJsonPath); os.IsNotExist(err) {
		msg = fmt.Sprintf("%v not find", configJsonPath)
		wLogger.Errorw(msg)
		return false, msg
	}

	// 打开并读取配置文件
	file, err := os.Open(configJsonPath)
	if err != nil {
		msg = fmt.Sprintf("failed to open %v: %v", configJsonPath, err)
		wLogger.Errorw(msg)
		return false, msg
	}
	defer file.Close()

	// 读取文件内容
	content, err := io.ReadAll(file)
	if err != nil {
		msg = fmt.Sprintf("failed to read %v: %v", configJsonPath, err)
		wLogger.Errorw(msg)
		return false, msg
	}

	// 解析JSON
	var config map[string]interface{}
	if err := json.Unmarshal(content, &config); err != nil {
		msg = fmt.Sprintf("invalid JSON format in %v: %v", configJsonPath, err)
		wLogger.Errorw(msg)
		return false, msg
	}

	// 检查是否包含base_model_name_or_path字段
	if _, exists := config["base_model_name_or_path"]; !exists {
		msg = fmt.Sprintf("missing base_model_name_or_path field in %v", configJsonPath)
		wLogger.Errorw(msg)
		return false, msg
	}

	// 检查base_model_name_or_path是否与pretrainedName匹配
	baseModelPath, ok := config["base_model_name_or_path"].(string)
	if !ok {
		msg = fmt.Sprintf("base_model_name_or_path is not a string in %v", configJsonPath)
		wLogger.Errorw(msg)
		return false, msg
	}

	if baseModelPath != pretrainedName {
		msg = fmt.Sprintf("base_model_name_or_path mismatch: expected %v, got %v", pretrainedName, baseModelPath)
		wLogger.Errorw(msg)
		return false, msg
	}

	wLogger.Infow("Lora validation successful", "pretrainedName", pretrainedName, "loraPath", loraPath)
	return true, ""
}

// WrapperUnloadRes 卸载资源
func WrapperUnloadRes(resId int) (err error) {
	wLogger.Infof("WrapperUnloadRes unload lora start", "resId", resId)
	resIdStr := strconv.Itoa(resId)
	patchIdMapMutex.Lock()
	patchRes, ok := patchResMap[resIdStr]
	if !ok {
		wLogger.Warnw("WrapperUnloadRes resId not loaded", "resId", resId)
		patchIdMapMutex.Unlock()
		return nil
	}
	patchIdMapMutex.Unlock()
	loraWeightPath := patchRes.LoraPath
	if _, err := os.Stat(loraWeightPath); os.IsNotExist(err) {
		wLogger.Errorw("WrapperLoadRes path does not exist", "resId", resId, "path", loraWeightPath)
		return fmt.Errorf("path does not exist: %s", loraWeightPath)
	}

	// 卸载lora
	unloadResp, err := PostRequest(fmt.Sprintf("%v/%v", server, unloadLoraAdapter), map[string]interface{}{
		"lora_name": patchRes.PatchId,
	})
	if err != nil {
		wLogger.Errorw("WrapperUnloadRes failed to unload lora", "resId", resId, "error", err)
		return fmt.Errorf("failed to unload lora: %v", err)
	}
	wLogger.Infof("WrapperUnloadRes unload lora success, resId:%v, unloadResp:%v", resId, string(unloadResp))

	// // 删除记录
	// os.Remove(filepath.Join("/home/.atp/lora_record", resIdStr))

	// 递归删除lora目录
	if err := os.RemoveAll(loraWeightPath); err != nil {
		wLogger.Errorw("WrapperUnloadRes failed to remove lora directory", "resId", resId, "path", loraWeightPath, "error", err)
		return err
	} else {
		wLogger.Infow("WrapperUnloadRes removed lora directory", "resId", resId, "path", loraWeightPath)
	}

	patchIdMapMutex.Lock()
	delete(patchResMap, resIdStr)
	delete(patchIdInnerPatchIdMap, patchRes.PatchId)
	patchIdMapMutex.Unlock()

	return
}

// WrapperDebugInfo 获取调试信息
func WrapperDebugInfo(hdl interface{}) (debug string) {
	return "debug info"
}

// WrapperSetCtrl 设置控制函数
func WrapperSetCtrl(fType comwrapper.CustomFuncType, f interface{}) (err error) {
	switch fType {
	case comwrapper.FuncTraceLog:
		traceFunc = f.(func(usrTag string, key string, value string) (code int))
		fmt.Println("WrapperSetCtrl traceLogFunc set successful.")
	case comwrapper.FuncMeter:
		meterFunc = f.(func(usrTag string, key string, count int) (code int))
		fmt.Println("WrapperSetCtrl meterFunc set successful.")
	case comwrapper.FuncLbExtra:
		LbExtraFunc = f.(func(params map[string]string) error)
		fmt.Println("WrapperSetCtrl FuncLbExtra set successful.")
	default:

	}
	return
}

func WrapperExec(usrTag string, params map[string]string, reqData []comwrapper.WrapperData) (respData []comwrapper.WrapperData, err error) {
	return nil, nil
}

// WrapperNotify 插件通知
func WrapperNotify(res comwrapper.WrapperData) (err error) {
	return nil
}

func parseFunctions(funcStr string) []*openai.FunctionDefinition {
	var functions []*openai.FunctionDefinition
	if err := json.Unmarshal([]byte(funcStr), &functions); err != nil {
		wLogger.Errorw("Invalid functions format", "functions", funcStr)
	}
	return functions
}

// parseMessages 解析消息
func parseMessages(prompt string) ([]Message, []openai.FunctionDefinition) {
	// 尝试解析JSON格式的消息
	var messages []Message
	if err := json.Unmarshal([]byte(prompt), &messages); err == nil {
		// 检查消息格式是否正确
		for _, msg := range messages {
			if msg.Role == "" || msg.Content == nil {
				wLogger.Errorw("parseMessages Invalid message format", "message", msg)
				return []Message{{
					Role:    "user",
					Content: prompt,
				}}, nil
			}
		}
		return messages, nil
	}
	wLogger.Debugw("parseMessages try sparkMsg")
	// 如果不是JSON格式，尝试解析为Spark格式
	var sparkMsg struct {
		Messages  []Message                   `json:"messages"`
		Functions []openai.FunctionDefinition `json:"functions"`
	}
	if err := json.Unmarshal([]byte(prompt), &sparkMsg); err == nil {
		// 直接返回所有消息，包括tool消息
		return sparkMsg.Messages, sparkMsg.Functions
	}

	// 如果都不是，则作为普通文本处理
	wLogger.Debugw("parseMessages Using plain text as message", "prompt", prompt)
	return []Message{{
		Role:    "user",
		Content: prompt,
	}}, nil
}

// formatMessages 格式化消息，支持搜索模板
func (inst *wrapperInst) formatMessages(prompt string, promptSearchTemplate string, promptSearchTemplateNoIndex string) ([]Message, []openai.FunctionDefinition) {
	messages, functions := parseMessages(prompt)
	wLogger.Debugf("formatMessages messages: %v\n, functions:%v", messages, functions)

	// 如果没有搜索模板，直接返回解析后的消息
	if promptSearchTemplate == "" && promptSearchTemplateNoIndex == "" {
		return messages, functions
	}

	lastMessage := &messages[len(messages)-1]
	if lastMessage.Role == "assistant" {
		if lastMessage.Prefix != nil && *lastMessage.Prefix {
			inst.continueFinalMessage = true
		}
	}

	// 查找最后一个tool消息
	var lastToolMsg *Message
	for i := len(messages) - 1; i >= 0; i-- {
		if messages[i].Role == "tool" {
			lastToolMsg = &messages[i]
			break
		}
	}
	wLogger.Debugf("formatMessages lastToolMsg: %v\n", lastToolMsg)

	// 如果没有tool消息，直接返回
	if lastToolMsg == nil {
		return messages, functions
	}

	// 解析tool消息内容
	var searchContent []map[string]interface{}
	if err := json.Unmarshal([]byte(lastToolMsg.Content.(string)), &searchContent); err != nil {
		wLogger.Errorw("Failed to parse tool message content", "error", err)
		return messages, functions
	}

	// 如果没有搜索内容，直接返回
	if len(searchContent) == 0 {
		return messages, functions
	}

	// 获取show_ref_label，默认为false
	showRefLabel := false
	if lastToolMsg.ShowRefLabel != nil {
		showRefLabel = *lastToolMsg.ShowRefLabel
	}

	// 格式化搜索内容
	var formattedContent []string
	for _, content := range searchContent {
		formattedText := fmt.Sprintf("[webpage %v begin]\n%v%v\n[webpage %v end]",
			content["index"],
			content["docid"],
			content["document"],
			content["index"])
		formattedContent = append(formattedContent, formattedText)
	}
	wLogger.Debugf("formatMessages formattedContent: %v\n", formattedContent)

	// 获取当前日期
	now := time.Now()
	weekdays := []string{"日", "一", "二", "三", "四", "五", "六"}
	currentDate := fmt.Sprintf("%d年%02d月%02d日星期%s",
		now.Year(), now.Month(), now.Day(), weekdays[now.Weekday()])

	// 查找最后一个用户消息并更新其内容
	for i := len(messages) - 1; i >= 0; i-- {
		if messages[i].Role == "user" {
			// 应用模板
			// 根据show_ref_label选择模板
			templateStr := promptSearchTemplate
			if !showRefLabel {
				templateStr = promptSearchTemplateNoIndex
			}

			// 创建模板
			tmpl, err := template.New("search").Parse(templateStr)
			if err != nil {
				wLogger.Errorw("Failed to parse template", "error", err)
				return messages, functions
			}

			// 准备模板数据
			data := struct {
				SearchResults string `json:"search_results"`
				CurDate       string `json:"cur_date"`
				Question      string `json:"question"`
			}{
				SearchResults: strings.Join(formattedContent, "\n"),
				CurDate:       currentDate,
				Question:      messages[i].Content.(string),
			}

			// 执行模板渲染
			var result strings.Builder
			if err := tmpl.Execute(&result, data); err != nil {
				wLogger.Errorw("Failed to execute template", "error", err)
				return messages, functions
			}

			messages[i].Content = result.String()
			break
		}
	}

	return messages, functions
}

// OpenAIClient OpenAI客户端
type OpenAIClient struct {
	openaiClient *openai.Client
}

// NewOpenAIClient 创建OpenAI客户端
func NewOpenAIClient(baseURL string) *OpenAIClient {
	config := openai.DefaultConfig("maas")
	config.BaseURL = baseURL
	config.HTTPClient = &http.Client{
		Timeout: streamContextTimeoutSeconds,
		Transport: &http.Transport{
			MaxIdleConns:        6000,
			MaxIdleConnsPerHost: 3000,
			MaxConnsPerHost:     3000,
			IdleConnTimeout:     90 * time.Second,
		},
	}

	return &OpenAIClient{
		openaiClient: openai.NewClientWithConfig(config),
	}
}

// ChatCompletionRequest 聊天完成请求
type ChatCompletionRequest struct {
	Model       string    `json:"model"`
	Messages    []Message `json:"messages"`
	Temperature float64   `json:"temperature,omitempty"`
	MaxTokens   int       `json:"max_tokens,omitempty"`
	Stream      bool      `json:"stream,omitempty"`
	Tools       []Tool    `json:"tools,omitempty"`
	ToolChoice  string    `json:"tool_choice,omitempty"`
}

// Message 消息结构
type Message struct {
	Role         string      `json:"role"`
	Content      interface{} `json:"content"`
	ShowRefLabel *bool       `json:"show_ref_label,omitempty"`
	Prefix       *bool       `json:"prefix,omitempty"` // for continue final message
}

// Tool 工具结构
type Tool struct {
	Type     string   `json:"type"`
	Function Function `json:"function"`
}

// Function 函数结构
type Function struct {
	Name        string                 `json:"name"`
	Description string                 `json:"description"`
	Parameters  map[string]interface{} `json:"parameters"`
}

// ChatCompletionResponse 聊天完成响应
type ChatCompletionResponse struct {
	ID      string   `json:"id"`
	Choices []Choice `json:"choices"`
	Usage   Usage    `json:"usage"`
}

// Choice 选择结构
type Choice struct {
	Index   int     `json:"index"`
	Message Message `json:"message"`
}

// Usage 使用量结构
type Usage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

// monitorSubprocess 监控子进程
func monitorSubprocess(cmd *exec.Cmd) {
	wLogger.Debugw("Starting Sglang process monitor...")

	// 等待进程结束
	err := cmd.Wait()
	if err != nil {
		fmt.Printf("Sglang process exited with error:%v\n", err)
		wLogger.Errorw("Sglang process exited with error", "error", err)
		// 这里可以添加重启逻辑或其他错误处理
	} else {
		fmt.Printf("Sglang process exited normally\n")
		wLogger.Debugw("Sglang process exited normally")
	}
}

// convertToOpenAIMessages 将自定义Message类型转换为OpenAI的ChatCompletionMessage类型
func convertToOpenAIMessages(messages []Message) []openai.ChatCompletionMessage {
	openAIMessages := make([]openai.ChatCompletionMessage, len(messages))
	for i, msg := range messages {
		content := ""
		if msg.Content != nil {
			switch v := msg.Content.(type) {
			case string:
				content = v
			default:
				// 如果不是字符串，尝试转换为JSON字符串
				if jsonBytes, err := json.Marshal(v); err == nil {
					content = string(jsonBytes)
				}
			}
		}
		openAIMessages[i] = openai.ChatCompletionMessage{
			Role:    msg.Role,
			Content: content,
		}
	}
	return openAIMessages
}

// WarmupPrompt 预热请求数据结构
type WarmupPrompt struct {
	Prompt      string  `json:"prompt"`
	MaxTokens   int     `json:"max_tokens"`
	Temperature float64 `json:"temperature"`
	Type        string  `json:"type"`
}

// WarmupRequest 发送预热请求
func WarmupRequest(url string, warmupDataPath string) error {
	wLogger.Infow("Starting warmup process", "url", url, "warmupDataPath", warmupDataPath)

	// 读取预热数据文件
	data, err := os.ReadFile(warmupDataPath)
	if err != nil {
		wLogger.Errorw("Failed to read warmup data file", "error", err, "path", warmupDataPath)
		return fmt.Errorf("failed to read warmup data file: %v", err)
	}

	// 解析JSON数据
	var warmupPrompts []WarmupPrompt
	if err := json.Unmarshal(data, &warmupPrompts); err != nil {
		wLogger.Errorw("Failed to parse warmup data", "error", err)
		return fmt.Errorf("failed to parse warmup data: %v", err)
	}

	if len(warmupPrompts) == 0 {
		wLogger.Warnw("No warmup prompts found")
		return nil
	}

	wLogger.Infow("Loaded warmup prompts", "count", len(warmupPrompts))

	// 发送预热请求
	successCount := 0
	totalCount := len(warmupPrompts)
	wg := sync.WaitGroup{}
	var successMutex sync.Mutex // 添加互斥锁保护successCount

	for i, prompt := range warmupPrompts {
		wg.Add(1)
		go func(i int, prompt WarmupPrompt) {
			defer wg.Done()
			// 构建请求payload
			payload := map[string]interface{}{
				"model": DEFAULT_MODEL_NAME,
				"messages": []map[string]interface{}{
					{
						"role":    "user",
						"content": prompt.Prompt,
					},
				},
				"max_tokens":  prompt.MaxTokens,
				"temperature": prompt.Temperature,
				"stream":      false,
			}

			// 发送请求
			startTime := time.Now()
			resp, err := PostRequest(url, payload)
			duration := time.Since(startTime)

			if err != nil {
				wLogger.Errorw("Warmup request failed", "index", i+1, "error", err, "prompt", prompt.Prompt)
			} else {
				// 使用互斥锁保护successCount的更新
				successMutex.Lock()
				successCount++
				successMutex.Unlock()
				wLogger.Infow("Warmup request successful", "index", i+1, "duration", duration, "response_length", len(resp))
			}
		}(i, prompt)
	}
	wg.Wait()

	successRate := float64(successCount) / float64(totalCount)
	wLogger.Infow("Warmup completed", "success_count", successCount, "total_count", totalCount, "success_rate", successRate)

	if successRate < 0.8 {
		wLogger.Errorw("Warmup success rate too low", "success_rate", successRate)
		return fmt.Errorf("warmup success rate too low: %.2f", successRate)
	}

	return nil
}

// PostRequest 发送POST请求（使用默认参数）
func PostRequest(url string, jsonData map[string]interface{}) ([]byte, error) {
	return PostRequestWithOptions(url, jsonData, nil, 3, 60*time.Second)
}

// PostRequestWithOptions 发送POST请求（自定义参数）
func PostRequestWithOptions(url string, jsonData map[string]interface{}, headers map[string]string, maxRetries int, timeout time.Duration) ([]byte, error) {
	// 创建HTTP客户端
	client := &http.Client{
		Timeout: timeout,
	}

	// 准备请求体
	var requestBody []byte
	var err error

	if jsonData != nil {
		requestBody, err = json.Marshal(jsonData)
		if err != nil {
			return nil, fmt.Errorf("failed to marshal json data: %v", err)
		}
	}

	// 重试机制
	var lastErr error
	for attempt := 0; attempt <= maxRetries; attempt++ {
		if attempt > 0 {
			wLogger.Debugw("Retrying POST request", "attempt", attempt, "url", url)
			time.Sleep(1 * time.Second)
		}

		// 每次重试都重新创建请求，确保请求体完整
		req, err := http.NewRequest("POST", url, bytes.NewBuffer(requestBody))
		if err != nil {
			lastErr = fmt.Errorf("failed to create request: %v", err)
			continue
		}

		// 设置默认Content-Type
		if jsonData != nil {
			req.Header.Set("Content-Type", "application/json")
		}

		// 设置自定义headers
		for key, value := range headers {
			req.Header.Set(key, value)
		}

		// 发送请求
		resp, err := client.Do(req)
		if logLevel == "debug" {
			errorMsg := ""
			if err != nil {
				errorMsg = err.Error()
			}
			wLogger.Debugw("Post process", "req", toString(req), "resp", toString(resp), "error", errorMsg)
		}

		if err != nil {
			lastErr = fmt.Errorf("request failed (attempt %d): %v", attempt+1, err)
			continue
		}

		// 读取响应
		body, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil {
			lastErr = fmt.Errorf("failed to read response body (attempt %d): %v", attempt+1, err)
			continue
		}

		// 检查HTTP状态码
		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			wLogger.Debugw("POST request successful", "url", url, "status", resp.StatusCode, "attempt", attempt+1)
			return body, nil
		}

		// 记录错误信息
		lastErr = fmt.Errorf("HTTP error (attempt %d): %s - %s", attempt+1, resp.Status, string(body))

		// 如果是4xx错误，不重试
		if resp.StatusCode >= 400 && resp.StatusCode < 500 {
			wLogger.Warnw("Client error, not retrying", "status", resp.StatusCode, "body", string(body))
			break
		}
	}

	return nil, fmt.Errorf("all retry attempts failed. Last error: %v", lastErr)
}

// readGenerationConfig 读取generation_config.json文件
func readGenerationConfig(baseModelPath string) (map[string]interface{}, error) {
	if baseModelPath == "" {
		return nil, fmt.Errorf("base model path is empty")
	}

	configPath := filepath.Join(baseModelPath, "generation_config.json")
	fmt.Printf("Reading generation config from: %s\n", configPath)

	// 检查文件是否存在
	if _, err := os.Stat(configPath); err != nil {
		return nil, fmt.Errorf("generation_config.json not found: %v", err)
	}

	// 读取文件内容
	configData, err := os.ReadFile(configPath)
	if err != nil {
		return nil, fmt.Errorf("failed to read generation_config.json: %v", err)
	}

	// 解析JSON
	var config map[string]interface{}
	if err := json.Unmarshal(configData, &config); err != nil {
		return nil, fmt.Errorf("failed to parse generation_config.json: %v", err)
	}

	// 提取配置信息
	genConfig := map[string]interface{}{}

	if temp, exists := config["temperature"]; exists {
		genConfig["temperature"] = temp
		fmt.Printf("Temperature from config: %v\n", temp)
	}

	if topP, exists := config["top_p"]; exists {
		genConfig["top_p"] = topP
		fmt.Printf("Top_p from config: %v\n", topP)
	}
	return genConfig, nil
}
