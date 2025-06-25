package main

import (
	"bufio"
	"comwrapper"
	"context"
	"encoding/json"
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

var (
	wLogger                     *utils.Logger
	logLevel                    = "debug"
	logCount                    = 10
	logSize                     = 30
	logAsync                    = true
	logPath                     = "/log/app/wrapper.log"
	respKey                     = "content"
	requestManager              *RequestManager
	httpServerPort              = 40000
	streamContextTimeoutSeconds = 1800 * time.Second
	temperature                 = 0.95
	maxTokens                   = 2048
	topP                        = 0.95
	promptSearchTemplate        string // 添加全局变量存储搜索模板
	promptSearchTemplateNoIndex string // 添加全局变量存储不带索引的搜索模板
	isReasoningModel            bool
	cmd                         *exec.Cmd
)

var traceFunc func(usrTag string, key string, value string) (code int)
var meterFunc func(usrTag string, key string, count int) (code int)
var LbExtraFunc func(params map[string]string) error

const (
	R1_THINK_START              = "<think>"
	R1_THINK_END                = "</think>"
	HTTP_SERVER_REQUEST_TIMEOUT = time.Second * 10
	HTTP_SERVER_MAX_RETRY_TIME  = time.Minute * 30
	STREAM_CONNECT_TIMEOUT      = "stream connect timeout"
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
	wLogger.Debugf("wrapper config:%v", cfg)

	// 获取搜索模板
	if promptSearchTemplate != "" {
		wLogger.Infow("Using custom prompt search template")
	} else {
		wLogger.Infow("No prompt search template provided")
	}

	// 从环境变量获取配置
	baseModel := os.Getenv("FULL_MODEL_PATH")

	isReasoningModelStr := os.Getenv("IS_REASONING_MODEL")
	if isReasoningModelStr == "true" {
		isReasoningModel = true
	}

	// 获取端口配置
	port := os.Getenv("HTTP_SERVER_PORT")
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
	extraArgs := os.Getenv("CMD_EXTRA_ARGS")
	if extraArgs == "" {
		extraArgs = "--mem-fraction-static 0.93 --torch-compile-max-bs 8 --max-running-requests 20"
		wLogger.Infow("Using default CMD_ARGS", "args", extraArgs)
	} else {
		wLogger.Infow("Using custom CMD_ARGS", "args", extraArgs)
	}

	// 检查是否启用指标
	enableMetrics := os.Getenv("ENABLE_METRICS")
	if enableMetrics == "true" {
		extraArgs += " --enable-metrics"
		wLogger.Infow("Metrics enabled")
	}

	// 检查是否启用tools
	toolCallParser := os.Getenv("TOOL_CALL_PARSER")
	if len(toolCallParser) > 0 {
		extraArgs += fmt.Sprintf(" --tool-call-parser %v ", toolCallParser)
	}

	// 检查多节点模式
	enableMultiNode := os.Getenv("ENABLE_MULTI_NODE_MODE")
	if enableMultiNode == "true" {
		wLogger.Infow("Multi-node mode enabled")

		// 获取组大小
		if groupSize := os.Getenv("LWS_GROUP_SIZE"); groupSize != "" {
			extraArgs += fmt.Sprintf(" --nnodes %s", groupSize)
		}

		// 获取leader地址
		if leaderAddr := os.Getenv("LWS_LEADER_ADDRESS"); leaderAddr != "" {

			distPort := os.Getenv("DIST_PORT")
			if distPort == "" {
				distPort = "20000"
			}
			extraArgs += fmt.Sprintf(" --dist-init-addr %s:%s", leaderAddr, distPort)
		}

		// 获取worker索引
		if workerIndex := os.Getenv("LWS_WORKER_INDEX"); workerIndex != "" {
			extraArgs += fmt.Sprintf(" --node-rank %s", workerIndex)
		}
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

	// 等待服务就绪
	if err := waitServerReady(fmt.Sprintf("http://localhost:%d/health", httpServerPort)); err != nil {
		return fmt.Errorf("server failed to start: %v", err)
	}

	// sglang指标上报
	//go reportSglangMetrics(fmt.Sprintf("http://localhost:%d/metrics", httpServerPort))

	// 初始化OpenAI客户端
	serverURL := fmt.Sprintf("http://127.0.0.1:%d/v1", httpServerPort)
	client := NewOpenAIClient(serverURL)

	// 初始化请求管理器并保存到全局变量
	requestManager = NewRequestManager(client)

	wLogger.Debugw("WrapperInit successful")
	return
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
	maxRetries := 0
	timeInerval := time.Second // 请求间隔
	for i := 0; timeInerval < HTTP_SERVER_MAX_RETRY_TIME; i++ {
		resp, err := httpClient.Get(serverURL)
		if err == nil && resp.StatusCode == http.StatusOK {
			wLogger.Debugw("Server ready", "url", serverURL)
			resp.Body.Close()
			return nil
		}

		if resp != nil {
			resp.Body.Close()
		}

		wLogger.Debugw("Server not ready, retrying...", "url", serverURL, "attempt", i+1)
		time.Sleep(timeInerval)
		timeInerval *= 2 // 每次重试增加间隔
		maxRetries++
	}
	return fmt.Errorf("server failed to start after %d attempts", maxRetries)
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
	usrTag     string
	sid        string
	client     *OpenAIClient
	stopQ      chan bool
	firstFrame bool
	callback   comwrapper.CallBackPtr
	params     map[string]string
	active     bool
	// errorCh    chan error
}

// WrapperCreate 插件会话实例创建
func WrapperCreate(usrTag string, params map[string]string, prsIds []int, cb comwrapper.CallBackPtr) (hdl unsafe.Pointer, err error) {
	sid := params["sid"]
	paramStr := ""
	for k, v := range params {
		paramStr += fmt.Sprintf("%s=%s;", k, v)
	}
	wLogger.Debugw("WrapperCreate start", "paramStr", paramStr, "sid", sid)

	// 创建新的实例
	inst := &wrapperInst{
		usrTag:     usrTag,
		sid:        sid,
		client:     requestManager.Client,
		stopQ:      make(chan bool, 2),
		firstFrame: true,
		callback:   cb,
		params:     params,
		active:     true, // 初始化active为true
		// errorCh:    make(chan error),
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
	wLogger.Debugf("WrapperWrite stream err:%v, sid:%v\n", err.Error(), inst.sid)
	errContent, _ := responseContent(comwrapper.DataBegin, 0, "", "", nil)

	if err1 := inst.callback(inst.usrTag, []comwrapper.WrapperData{errContent}, err); err1 != nil {
		wLogger.Errorw("WrapperWrite error callback failed", "error", err, "sid", inst.sid)
	}
	// inst.errorCh <- err
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
	wLogger.Debugf("WrapperWrite stream responseEnd index:%v, prompt_tokens_len:%v, result_tokens_len:%v,responseData:%v, sid:%v\n", index, prompt_tokens_len, result_tokens_len, responseData, inst.sid)
	if err = inst.callback(inst.usrTag, responseData, nil); err != nil {
		wLogger.Errorw("WrapperWrite usage callback error", "error", err, "sid", inst.sid)
		return err
	}
	return nil
}

func toString(v any) string {
	res, _ := json.Marshal(v)
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
	LogitBias map[string]int `json:"logit_bias,omitempty"`
}

func openaiFunctionCall(inst *wrapperInst, functions []openai.FunctionDefinition, req *openai.ChatCompletionRequest) error {
	openaiTools := make([]openai.Tool, len(functions))
	for i, f := range functions {
		openaiTools[i] = openai.Tool{
			Type:     "function",
			Function: &f,
		}
	}
	req.Tools = openaiTools
	req.ToolChoice = "auto"
	//req.Stream = false
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

// WrapperWrite 数据写入
func WrapperWrite(hdl unsafe.Pointer, req []comwrapper.WrapperData) (err error) {
	inst := (*wrapperInst)(hdl)
	wLogger.Debugf("WrapperWrite inst:%v, req:%v\n", toString(inst), toString(req))
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

		wLogger.Debugw("WrapperWrite processing data", "data", string(v.Data), "status", v.Status, "sid", inst.sid)

		// 从params中获取参数, 否则使用默认值
		if temp, ok := inst.params["temperature"]; ok {
			if t, err := strconv.ParseFloat(temp, 64); err == nil {
				temperature = t
			}
		}

		if tokens, ok := inst.params["max_tokens"]; ok {
			if t, err := strconv.Atoi(tokens); err == nil {
				maxTokens = t
			}
		}

		if tp, ok := inst.params["top_p"]; ok {
			if t, err := strconv.ParseFloat(tp, 32); err == nil {
				topP = t
			}
		}

		streamReq := &openai.ChatCompletionRequest{
			Model: "default",
		}

		streamReq.ExtraBody = map[string]any{
			"chat_template_kwargs": map[string]interface{}{
				"enable_thinking": false,
			},
		}
		if enable_thinking, ok := inst.params["enable_thinking"]; ok {
			if enable_thinking == "true" {
				streamReq.ExtraBody = map[string]any{
					"chat_template_kwargs": map[string]interface{}{
						"enable_thinking": true,
					},
				}
			}
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
		wLogger.Infow("WrapperWrite request parameters",
			"sid", inst.sid,
			"temperature", temperature,
			"maxTokens", maxTokens,
			"topP", topP)

		promptTokensLen, resultTokensLen := 0, 0
		openaiMsgs, functions := formatMessages(string(v.Data), promptSearchTemplate, promptSearchTemplateNoIndex)
		streamReq.Messages = convertToOpenAIMessages(openaiMsgs)
		// 如果是fuction请求
		if len(functions) > 0 {
			return openaiFunctionCall(inst, functions, streamReq)
		}

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

		streamReq.Temperature = float32(temperature)
		streamReq.MaxTokens = maxTokens
		streamReq.Stream = true
		streamReq.StreamOptions = &openai.StreamOptions{
			IncludeUsage: true,
		}
		streamReq.TopP = float32(topP)
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
		wLogger.Debugf("WrapperWrite streamReq:%v, %v\n", toString(streamReq), inst.sid)

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

			status = comwrapper.DataContinue
			// 首帧返回空
			firstFrameContent, err := responseContent(status, index, "", "", nil)
			if err != nil {
				wLogger.Errorw("WrapperWrite error fisrt callback", "error", err, "sid", inst.sid)
				responseError(inst, err)
				return
			}
			wLogger.Debugf("fisrtFrameContent:%v index:%v,status:%v sid:%v\n", "", index, status, inst.sid)
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
						responseError(inst, err)
						wLogger.Errorw("WrapperWrite read stream error", "error", err, "sid", inst.sid)
						return
					}

					// 处理content数据
					if len(response.Choices) > 0 && response.Choices[0].Delta.Content != "" {
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
								wLogger.Errorw("WrapperWrite usage callback error", "error", err, "sid", inst.sid)
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
								wLogger.Errorw("WrapperWrite usage callback error", "error", err, "sid", inst.sid)
								return
							}
							index += 1
							continue
						}

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
							wLogger.Errorw("WrapperWrite usage callback error", "error", err, "sid", inst.sid)
							return
						}
						index += 1
					}

					// 处理usage数据
					if response.Usage != nil {
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
		// select {
		// case err := <-inst.errorCh:
		// 	return err
		// case <-time.After(1 * time.Second):
		// 	return nil
		// }

	}

	return nil
}

// WrapperDestroy 会话资源销毁
func WrapperDestroy(hdl interface{}) (err error) {
	inst := (*wrapperInst)(hdl.(unsafe.Pointer))
	wLogger.Debugw("WrapperDestroy", "sid", inst.sid)
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
			wLogger.Debugf("WrapperFini err:%v", err)
		} else {
			fmt.Printf("WrapperFini cmd kill success\n")
			wLogger.Debugf("WrapperFini cmd kill success")
		}
	}
	wLogger.Debugf("WarpperFini success")
	return
}

// WrapperVersion 获取版本信息
func WrapperVersion() (version string) {
	return
}

// WrapperLoadRes 加载资源
func WrapperLoadRes(res comwrapper.WrapperData, resId int) (err error) {
	return
}

// WrapperUnloadRes 卸载资源
func WrapperUnloadRes(resId int) (err error) {
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
func formatMessages(prompt string, promptSearchTemplate string, promptSearchTemplateNoIndex string) ([]Message, []openai.FunctionDefinition) {
	messages, functions := parseMessages(prompt)
	wLogger.Debugf("formatMessages messages: %v\n, functions:%v", messages, functions)

	// 如果没有搜索模板，直接返回解析后的消息
	if promptSearchTemplate == "" && promptSearchTemplateNoIndex == "" {
		return messages, functions
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
		Timeout: 30 * time.Minute,
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
