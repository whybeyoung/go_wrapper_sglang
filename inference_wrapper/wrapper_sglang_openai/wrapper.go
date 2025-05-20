package main

import (
	"bufio"
	"comwrapper"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
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
	httpServerPort              int
	isDecodeMode                bool   // 添加全局变量存储PD模式
	isNeedPDInfo                bool   // 不分离不需要PD信息
	promptSearchTemplate        string // 添加全局变量存储搜索模板
	promptSearchTemplateNoIndex string // 添加全局变量存储不带索引的搜索模板
	isReasoningModel            bool
	pretrainedName              string // 预训练模型名称
	finetuneType                string // 微调类型
)

const (
	R1_THINK_START = "<think>"
	R1_THINK_END   = "</think>"
)

// RequestManager 请求管理器
type RequestManager struct {
	Client      *OpenAIClient
	Logger      *utils.Logger
	RequestLock sync.RWMutex
	IDAllocator *CircularIDAllocator
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
		Client:      client,
		Logger:      logger,
		IDAllocator: NewCircularIDAllocator("", 0),
	}
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

	// 获取搜索模板
	if promptSearchTemplate != "" {
		wLogger.Infow("Using custom prompt search template")
	} else {
		wLogger.Infow("No prompt search template provided")
	}

	// 从环境变量获取配置
	baseModel := os.Getenv("FULL_MODEL_PATH")
	pretrainedName = os.Getenv("PRETRAINED_MODEL_NAME")
	if pretrainedName == "" {
		return fmt.Errorf("PRETRAINED_MODEL_NAME is not set")
	}
	finetuneType = os.Getenv("FINETUNE_TYPE")
	if finetuneType == "" {
		finetuneType = "lora"
	}
	isReasoningModelStr := os.Getenv("IS_REASONING_MODEL")
	if isReasoningModelStr == "true" {
		isReasoningModel = true
	}

	// 获取端口配置
	port := os.Getenv("HTTP_SERVER_PORT")
	if port == "" {
		port = "40000"
	}
	httpServerPort, _ = strconv.Atoi(port)

	// 获取额外参数
	extraArgs := os.Getenv("CMD_EXTRA_ARGS")
	if extraArgs == "" {
		extraArgs = "--tp 8 --mem-fraction-static 0.93 --torch-compile-max-bs 8 --max-running-requests 20"
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

	// 检查PD模式
	pdType := os.Getenv("AIGES_PD_ROLE")
	isDecodeMode = true // 默认为decode模式
	isNeedPDInfo = true // 默认位需要PD信息
	if pdType == "prefill" {
		isDecodeMode = false
		extraArgs += " --disaggregation-mode prefill"
		wLogger.Infow("Running in prefill mode")
	} else if pdType == "decode" {
		extraArgs += " --disaggregation-mode decode"
		wLogger.Infow("Running in decode mode")
	} else {
		isNeedPDInfo = false
		wLogger.Infow("Running in prefill-decode mode")
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
	}
	// 添加额外参数
	args = append(args, strings.Fields(extraArgs)...)

	// 启动sglang服务
	cmd := exec.Command("python", args...)
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

	// 初始化OpenAI客户端
	serverURL := fmt.Sprintf("http://127.0.0.1:%d/v1", httpServerPort)
	client := NewOpenAIClient(serverURL)

	// 初始化请求管理器并保存到全局变量
	requestManager = NewRequestManager(client)

	wLogger.Debugw("WrapperInit successful")
	return
}

// waitServerReady 等待服务器就绪
func waitServerReady(serverURL string) error {
	maxRetries := 3000 // 最多重试30次
	for i := 0; i < maxRetries; i++ {
		resp, err := http.Get(serverURL)
		if err == nil && resp.StatusCode == http.StatusOK {
			wLogger.Debugw("Server ready", "url", serverURL)
			resp.Body.Close()
			return nil
		}

		if resp != nil {
			resp.Body.Close()
		}

		wLogger.Debugw("Server not ready, retrying...", "url", serverURL, "attempt", i+1)
		time.Sleep(time.Second)
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
	}

	wLogger.Infow("WrapperCreate successful", "sid", sid, "usrTag", usrTag)
	return unsafe.Pointer(inst), nil
}

// PDInfo 结构体用于存储PD相关信息
type PDInfo struct {
	BootstrapIP   string `json:"bootstrap_ip"`
	BootstrapPort int    `json:"bootstrap_port"`
	BootstrapRoom int64  `json:"bootstrap_room"`
	PrefillAddr   string `json:"prefill_addr"`
}

// getPDInfo 获取PD相关信息
func getPDInfo() *PDInfo {
	bootstrapIP := getLocalIP()
	bootstrapPort := 8998 // 默认端口
	if port := os.Getenv("PD_BOOTSTRAP_PORT"); port != "" {
		if p, err := strconv.Atoi(port); err == nil {
			bootstrapPort = p
		}
	}

	roomID := requestManager.IDAllocator.Allocate()
	prefillAddr := fmt.Sprintf("%s:%d", bootstrapIP, httpServerPort)

	return &PDInfo{
		BootstrapIP:   bootstrapIP,
		BootstrapPort: bootstrapPort,
		BootstrapRoom: roomID,
		PrefillAddr:   prefillAddr,
	}
}

func responseContent(status comwrapper.DataStatus, index int, text string, resoning_content string) (*comwrapper.WrapperData, error) {
	result := map[string]interface{}{
		"choice": []map[string]interface{}{
			{
				"content":           text,
				"reasoning_content": resoning_content,
				"index":             index,
				"role":              "assistant",
			},
		},
		"question_type": "",
	}
	data, err := json.Marshal(result)
	if err != nil {
		return nil, err
	}
	return &comwrapper.WrapperData{
		Key:      "content",
		Data:     data,
		Desc:     nil,
		Encoding: "utf-8",
		Type:     comwrapper.DataText,
		Status:   status,
	}, nil
}
func responseUsage(status comwrapper.DataStatus, prompt_token, completion_token int) (*comwrapper.WrapperData, error) {
	result := map[string]interface{}{
		"usage": map[string]interface{}{
			"prompt_tokens":     prompt_token,
			"completion_tokens": completion_token,
			"total_tokens":      prompt_token + completion_token,
			"question_tokens":   0,
		},
	}
	data, err := json.Marshal(result)
	if err != nil {
		return nil, err
	}
	return &comwrapper.WrapperData{
		Key:      "usage",
		Data:     data,
		Desc:     nil,
		Encoding: "utf-8",
		Type:     comwrapper.DataText,
		Status:   status,
	}, nil
}
func toString(v any) string {
	bytes, _ := json.Marshal(v)
	return string(bytes)
}

// WrapperWrite 数据写入
func WrapperWrite(hdl unsafe.Pointer, req []comwrapper.WrapperData) (err error) {
	inst := (*wrapperInst)(hdl)
	wLogger.Debugw(fmt.Sprintf("WrapperWrite inst:%v\n", toString(inst)))
	wLogger.Debugw(fmt.Sprintf("WrapperWrite req:%v\n", toString(req)))
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

	// patch_id := inst.params["patch_id"]
	// if len(patch_id) == 0 {
	// 	patch_id= "0"
	// }

	// model_name := pretrainedName
	// var lora_path string
	// if finetuneType == "lora" || finetuneType == "qlora"{
	// 	if patch_id != "0" {
	// }

	for _, v := range req {
		if v.Key == "__kv_info" {
			continue // 跳过kv_info数据
		}

		wLogger.Debugw("WrapperWrite processing data", "data", string(v.Data), "status", v.Status, "sid", inst.sid)

		// 从params中获取参数
		temperature := 0.95 // 默认值
		if temp, ok := inst.params["temperature"]; ok {
			if t, err := strconv.ParseFloat(temp, 64); err == nil {
				temperature = t
			}
		}

		maxTokens := 2048 // 默认值
		if tokens, ok := inst.params["max_tokens"]; ok {
			if t, err := strconv.Atoi(tokens); err == nil {
				maxTokens = t
			}
		}

		topP := 0.95 // 默认值
		if tp, ok := inst.params["top_p"]; ok {
			if t, err := strconv.ParseFloat(tp, 32); err == nil {
				topP = t
			}
		}

		wLogger.Infow("WrapperWrite request parameters",
			"sid", inst.sid,
			"temperature", temperature,
			"maxTokens", maxTokens,
			"topP", topP)

		promptTokensLen, resultTokensLen := 0, 0
		openaiMsgs := formatMessages(string(v.Data), promptSearchTemplate, promptSearchTemplateNoIndex)
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

		// 创建流式请求
		streamReq := &openai.ChatCompletionRequest{
			Model:       "default",
			Messages:    convertToOpenAIMessages(openaiMsgs),
			Temperature: float32(temperature),
			MaxTokens:   maxTokens,
			Stream:      true,
			StreamOptions: &openai.StreamOptions{
				IncludeUsage: true,
			},
			TopP: float32(topP),
		}
		wLogger.Debugw(fmt.Sprintf("WrapperWrite streamReq:%v\n", toString(streamReq)), "sid", inst.sid)

		// 使用协程处理流式请求
		go func(req *openai.ChatCompletionRequest, status comwrapper.DataStatus) {
			wLogger.Infow("WrapperWrite starting stream inference", "sid", inst.sid)
			// startTime := time.Now()
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
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
			firstFrameContent, err := responseContent(status, index, "", "")
			if err != nil {
				wLogger.Errorw("WrapperWrite error fisrt callback", "error", err, "sid", inst.sid)
				responseError(inst, err)
				return
			}
			wLogger.Debugw(fmt.Sprintf("fisrtFrameContent:%v", toString(firstFrameContent)), "sid", inst.sid)
			if err := inst.callback(inst.usrTag, []comwrapper.WrapperData{*firstFrameContent}, nil); err != nil {
				wLogger.Errorw("WrapperWrite error callback failed", "error", err, "sid", inst.sid)
				return
			}
			index += 1

			for {
				select {
				case <-ctx.Done():
					if ctx.Err() == context.DeadlineExceeded {
						wLogger.Warnw("WrapperWrite stream timeout", "sid", inst.sid)
						goto endLoop
					}
					return
				default:
					// 创建响应数据切片
					responseData := make([]comwrapper.WrapperData, 0, 3) // 预分配容量为2

					response, err := stream.Recv()

					if err != nil {
						if err == io.EOF {
							wLogger.Infow("WrapperWrite stream ended normally", "sid", inst.sid)
							goto endLoop
						}
						wLogger.Errorw("WrapperWrite read stream error", "error", err, "sid", inst.sid)
						return
					}

					// 处理content数据
					if len(response.Choices) > 0 && response.Choices[0].Delta.Content != "" {
						var content *comwrapper.WrapperData
						chunk_content := response.Choices[0].Delta.Content
						if chunk_content == R1_THINK_START {
							thinking = true
							continue
						}
						if strings.Contains(chunk_content, R1_THINK_END) {
							// content的内容是</think>，有可能出来2帧
							thinking = false
							chunks := strings.Split(chunk_content, R1_THINK_END)

							if len(chunks) != 2 {
								wLogger.Errorw("WrapperWrite chunks length is not 2", "chunks", chunks)
								responseError(inst, err)
								return
							}
							reasoning_last_chunk := chunks[0]
							answer_start_chunk := chunks[1]
							reasoning_last_chunk_content, err := responseContent(status, index, "", reasoning_last_chunk)
							wLogger.Debugw(fmt.Sprintf("WrapperWrite stream response reasoning_last_chunk:%v\n", reasoning_last_chunk_content), "sid", inst.sid)
							if err != nil {
								wLogger.Errorw("WrapperWrite reasoning_last_chunk error", "error", err, "sid", inst.sid)
								responseError(inst, err)
								return
							}
							fullContent += reasoning_last_chunk
							responseData = []comwrapper.WrapperData{*reasoning_last_chunk_content}
							if err = inst.callback(inst.usrTag, responseData, nil); err != nil {
								wLogger.Errorw("WrapperWrite usage callback error", "error", err, "sid", inst.sid)
								return
							}
							index += 1

							answer_start_chunk_content, err := responseContent(status, index, answer_start_chunk, "")
							if err != nil {
								wLogger.Errorw("WrapperWrite answer_start_chunk error", "error", err, "sid", inst.sid)
								responseError(inst, err)
								return
							}
							wLogger.Debugw(fmt.Sprintf("WrapperWrite stream response answer_start_chunk:%v\n", answer_start_chunk_content), "sid", inst.sid)
							fullContent += answer_start_chunk
							responseData = []comwrapper.WrapperData{*answer_start_chunk_content}
							if err = inst.callback(inst.usrTag, responseData, nil); err != nil {
								wLogger.Errorw("WrapperWrite usage callback error", "error", err, "sid", inst.sid)
								return
							}
							index += 1
							continue
						}

						if thinking {
							content, err = responseContent(status, index, "", chunk_content)
							if err != nil {
								wLogger.Errorw("WrapperWrite thinking content error", "error", err, "sid", inst.sid)
								responseError(inst, err)
								return
							}
						} else {
							content, err = responseContent(status, index, chunk_content, "")
							if err != nil {
								wLogger.Errorw("WrapperWrite content error", "error", err, "sid", inst.sid)
								responseError(inst, err)
								return
							}
						}
						responseData = append(responseData, *content)
						fullContent += chunk_content
						index += 1
						wLogger.Debugw(fmt.Sprintf("WrapperWrite stream response think:%v, index:%v, responseData:%v\n", thinking, index, toString(responseData)), "sid", inst.sid)
						if err = inst.callback(inst.usrTag, responseData, nil); err != nil {
							wLogger.Errorw("WrapperWrite usage callback error", "error", err, "sid", inst.sid)
							return
						}
					}

					// 处理usage数据
					if response.Usage != nil {
						// 创建usage数据结构
						promptTokensLen = response.Usage.PromptTokens
						resultTokensLen = response.Usage.CompletionTokens
						wLogger.Debugw(fmt.Sprintf("WrapperWrite stream responseEnd index:%v, promptTokensLen:%v, resultTokensLen:%v\n", index, promptTokensLen, resultTokensLen), "sid", inst.sid)
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
func responseError(inst *wrapperInst, err error) {
	if inst == nil {
		wLogger.Errorw("WrapperWrite instance is nil", "error", err)
		return
	}
	wLogger.Errorw("WrapperWrite stream error", "error", err, "sid", inst.sid)
	// 发送错误响应
	errorContent := comwrapper.WrapperData{
		Key:      "content",
		Data:     []byte(fmt.Sprintf(`{"error": "%v"}`, err)),
		Desc:     nil,
		Encoding: "utf-8",
		Type:     comwrapper.DataText,
		Status:   comwrapper.DataEnd,
	}
	wLogger.Debugw(fmt.Sprintf("WrapperWrite stream errorContent:%v\n", toString(errorContent)), "sid", inst.sid)
	if err := inst.callback(inst.usrTag, []comwrapper.WrapperData{errorContent}, nil); err != nil {
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
	content, err := responseContent(status, index, "", "")
	if err != nil {
		responseError(inst, err)
		return err
	}
	responseData := []comwrapper.WrapperData{*content, *usageWrapperData}
	wLogger.Debugw(fmt.Sprintf("WrapperWrite stream responseEnd index:%v, prompt_tokens_len:%v, result_tokens_len:%v,responseData:%v\n", index, prompt_tokens_len, result_tokens_len, toString(responseData)), "sid", inst.sid)
	if err = inst.callback(inst.usrTag, responseData, nil); err != nil {
		wLogger.Errorw("WrapperWrite usage callback error", "error", err, "sid", inst.sid)
		return err
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
	return nil
}

// parseMessages 解析消息
func parseMessages(prompt string) []Message {
	// 尝试解析JSON格式的消息
	var messages []Message
	if err := json.Unmarshal([]byte(prompt), &messages); err == nil {
		// 检查消息格式是否正确
		for _, msg := range messages {
			if msg.Role == "" || msg.Content == nil {
				wLogger.Errorw("Invalid message format", "message", msg)
				return []Message{{
					Role:    "user",
					Content: prompt,
				}}
			}
		}
		return messages
	}

	// 如果不是JSON格式，尝试解析为Spark格式
	var sparkMsg struct {
		Messages []Message `json:"messages"`
	}
	if err := json.Unmarshal([]byte(prompt), &sparkMsg); err == nil {
		// 直接返回所有消息，包括tool消息
		return sparkMsg.Messages
	}

	// 如果都不是，则作为普通文本处理
	wLogger.Debugw("Using plain text as message", "prompt", prompt)
	return []Message{{
		Role:    "user",
		Content: prompt,
	}}
}

// formatMessages 格式化消息，支持搜索模板
func formatMessages(prompt string, promptSearchTemplate string, promptSearchTemplateNoIndex string) []Message {
	messages := parseMessages(prompt)
	wLogger.Debugw(fmt.Sprintf("formatMessages messages: %v\n", messages))

	// 如果没有搜索模板，直接返回解析后的消息
	if promptSearchTemplate == "" && promptSearchTemplateNoIndex == "" {
		return messages
	}

	// 查找最后一个tool消息
	var lastToolMsg *Message
	for i := len(messages) - 1; i >= 0; i-- {
		if messages[i].Role == "tool" {
			lastToolMsg = &messages[i]
			break
		}
	}
	wLogger.Debugw(fmt.Sprintf("formatMessages lastToolMsg: %v\n", lastToolMsg))

	// 如果没有tool消息，直接返回
	if lastToolMsg == nil {
		return messages
	}

	// 解析tool消息内容
	var searchContent []map[string]interface{}
	if err := json.Unmarshal([]byte(lastToolMsg.Content.(string)), &searchContent); err != nil {
		wLogger.Errorw("Failed to parse tool message content", "error", err)
		return messages
	}

	// 如果没有搜索内容，直接返回
	if len(searchContent) == 0 {
		return messages
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
	wLogger.Debugw(fmt.Sprintf("formatMessages formattedContent: %v\n", formattedContent))

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
				return messages
			}

			// 准备模板数据
			data := struct {
				SearchResults string
				CurDate       string
				Question      string
			}{
				SearchResults: strings.Join(formattedContent, "\n"),
				CurDate:       currentDate,
				Question:      messages[i].Content.(string),
			}

			// 执行模板渲染
			var result strings.Builder
			if err := tmpl.Execute(&result, data); err != nil {
				wLogger.Errorw("Failed to execute template", "error", err)
				return messages
			}

			messages[i].Content = result.String()
			break
		}
	}

	return messages
}

func WrapperExec(usrTag string, params map[string]string, reqData []comwrapper.WrapperData) (respData []comwrapper.WrapperData, err error) {
	return nil, nil
}

// CircularIDAllocator ID分配器
type CircularIDAllocator struct {
	MinID     int64
	MaxID     int64
	CurrentID int64
	IP        string
	Port      int
	IPHash    int64
	Offset    int64
	Lock      sync.Mutex
}

// NewCircularIDAllocator 创建ID分配器
func NewCircularIDAllocator(mockIP string, port int) *CircularIDAllocator {
	ip := mockIP
	if ip == "" {
		ip = getLocalIP()
	}

	ipHash := hashIP(ip)
	offset := (ipHash * (1 << 32)) + (int64(port) * (1 << 16))

	return &CircularIDAllocator{
		MinID:     0,
		MaxID:     math.MaxInt64,
		CurrentID: 0,
		IP:        ip,
		Port:      port,
		IPHash:    ipHash,
		Offset:    offset,
	}
}

// Allocate 分配新ID
func (a *CircularIDAllocator) Allocate() int64 {
	a.Lock.Lock()
	defer a.Lock.Unlock()

	allocatedID := a.CurrentID + a.Offset
	a.CurrentID++

	if a.CurrentID >= (1 << 32) {
		a.CurrentID = 0
	}

	return allocatedID
}

// hashIP IP地址哈希
func hashIP(ip string) int64 {
	parts := strings.Split(ip, ".")
	hash := int64(0)

	for _, part := range parts {
		num, _ := strconv.ParseInt(part, 10, 64)
		hash = (hash*256 + num) % (1 << 32)
	}

	return hash
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
	// PD相关参数
	BootstrapHost string `json:"bootstrap_host,omitempty"`
	BootstrapPort int    `json:"bootstrap_port,omitempty"`
	BootstrapRoom int64  `json:"bootstrap_room,omitempty"`
	PrefillAddr   string `json:"prefill_addr,omitempty"`
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
		wLogger.Errorw("Sglang process exited with error", "error", err)
		// 这里可以添加重启逻辑或其他错误处理
	} else {
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

func WrapperNotify(res comwrapper.WrapperData) (err error) {
	return nil
}
