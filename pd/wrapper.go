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
	"strconv"
	"strings"
	"sync"
	"time"
	"unsafe"

	"git.iflytek.com/AIaaS/xsf/utils"
)

var (
	wLogger              *utils.Logger
	logLevel             = "debug"
	logCount             = 10
	logSize              = 30
	logAsync             = true
	logPath              = "/log/app/wrapper.log"
	respKey              = "content"
	requestManager       *RequestManager
	httpServerPort       int
	isDecodeMode         bool   // 添加全局变量存储PD模式
	promptSearchTemplate string // 添加全局变量存储搜索模板
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
	fmt.Println("---- wrapper init ----")
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

	if promptSearchTemplate != "" {
		wLogger.Infow("Using custom prompt search template")
	} else {
		wLogger.Infow("No prompt search template provided")
	}

	// 从环境变量获取配置
	baseModel := os.Getenv("FULL_MODEL_PATH")

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
	if pdType == "prefill" {
		isDecodeMode = false
		extraArgs += " --disaggregation-mode prefill"
		wLogger.Infow("Running in prefill mode")
	} else if pdType == "decode" {
		extraArgs += " --disaggregation-mode decode"
		wLogger.Infow("Running in decode mode")
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

// WrapperWrite 数据写入
func WrapperWrite(hdl unsafe.Pointer, req []comwrapper.WrapperData) (err error) {
	inst := (*wrapperInst)(hdl)

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

	var pdInfo *PDInfo

	// 如果是prefill模式，生成PD信息并发送kv_info
	if !isDecodeMode {
		pdInfo = getPDInfo()
		wLogger.Infow("WrapperWrite generated PD info",
			"sid", inst.sid,
			"bootstrapIP", pdInfo.BootstrapIP,
			"bootstrapPort", pdInfo.BootstrapPort,
			"bootstrapRoom", pdInfo.BootstrapRoom,
			"prefillAddr", pdInfo.PrefillAddr)

		// 创建kv_info响应
		kvInfo := map[string]interface{}{
			"bootstrap_ip":   pdInfo.BootstrapIP,
			"bootstrap_room": pdInfo.BootstrapRoom,
			"prefill_addr":   pdInfo.PrefillAddr,
			"bootstrap_port": pdInfo.BootstrapPort,
		}

		data, err := json.Marshal(kvInfo)
		if err != nil {
			wLogger.Errorw("WrapperWrite marshal kv_info error", "error", err, "sid", inst.sid)
			return err
		}

		// 发送kv_info
		kvContent := comwrapper.WrapperData{
			Key:      "__kv_info",
			Data:     data,
			Desc:     nil,
			Encoding: "utf-8",
			Type:     comwrapper.DataText,
			Status:   comwrapper.DataEnd,
		}

		// 查找messages数据
		var messagesData []byte
		for _, v := range req {
			if v.Key == "messages" {
				messagesData = v.Data
				break
			}
		}

		if messagesData == nil {
			wLogger.Errorw("WrapperWrite no messages data found", "sid", inst.sid)
			return fmt.Errorf("no messages data found")
		}

		// 发送messages
		messagesContent := comwrapper.WrapperData{
			Key:      "messages",
			Data:     messagesData,
			Desc:     nil,
			Encoding: "utf-8",
			Type:     comwrapper.DataText,
			Status:   comwrapper.DataEnd,
		}

		// 发送keepalive_down begin
		keepAliveBegin := comwrapper.WrapperData{
			Key:      "__keepalive_down",
			Data:     []byte{},
			Desc:     nil,
			Encoding: "utf-8",
			Type:     comwrapper.DataText,
			Status:   comwrapper.DataBegin,
		}

		// 按顺序发送所有数据
		if err = inst.callback(inst.usrTag, []comwrapper.WrapperData{kvContent, messagesContent, keepAliveBegin}, nil); err != nil {
			wLogger.Errorw("WrapperWrite callback error", "error", err, "sid", inst.sid)
			return err
		}
		wLogger.Infow("WrapperWrite sent kv_info, messages and keepalive_down begin", "sid", inst.sid)
	}
	if isDecodeMode {
		// 如果是decode模式，从kv_info中获取PD信息
		for _, v := range req {
			if v.Key == "__kv_info" {
				if err := json.Unmarshal(v.Data, &pdInfo); err != nil {
					wLogger.Errorw("WrapperWrite unmarshal kv_info error", "error", err, "sid", inst.sid)
					return err
				}
				wLogger.Infow("WrapperWrite received PD info from kv_info",
					"sid", inst.sid,
					"bootstrapIP", pdInfo.BootstrapIP,
					"bootstrapPort", pdInfo.BootstrapPort,
					"bootstrapRoom", pdInfo.BootstrapRoom,
					"prefillAddr", pdInfo.PrefillAddr)
				break
			}
		}
	}

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

		wLogger.Infow("WrapperWrite request parameters",
			"sid", inst.sid,
			"temperature", temperature,
			"maxTokens", maxTokens)

		// 创建流式请求
		streamReq := &openai.ChatCompletionRequest{
			Model:       "default",
			Messages:    convertToOpenAIMessages(formatMessages(string(v.Data), promptSearchTemplate)),
			Temperature: float32(temperature),
			MaxTokens:   maxTokens,
			Stream:      true,
			StreamOptions: &openai.StreamOptions{
				IncludeUsage: true,
			},
			// 添加PD相关参数
			ExtraBody: map[string]interface{}{
				"bootstrap_host": pdInfo.BootstrapIP,
				"bootstrap_port": pdInfo.BootstrapPort,
				"bootstrap_room": pdInfo.BootstrapRoom,
				"prefill_addr":   pdInfo.PrefillAddr,
				"session_params": map[string]interface{}{
					"rid": inst.sid,
				},
			},
		}

		// 使用协程处理流式请求
		go func(req *openai.ChatCompletionRequest, status comwrapper.DataStatus) {
			wLogger.Infow("WrapperWrite starting stream inference", "sid", inst.sid)

			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
			defer cancel()

			stream, err := inst.client.openaiClient.CreateChatCompletionStream(ctx, *req)
			if err != nil {
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
				if err := inst.callback(inst.usrTag, []comwrapper.WrapperData{errorContent}, nil); err != nil {
					wLogger.Errorw("WrapperWrite error callback failed", "error", err, "sid", inst.sid)
				}
				return
			}
			defer stream.Close()

			index := 0
			done := make(chan struct{})
			last := false

			for {
				select {
				case <-ctx.Done():
					if ctx.Err() == context.DeadlineExceeded {
						wLogger.Warnw("WrapperWrite stream timeout", "sid", inst.sid)
						goto endLoop
					}
					close(done)
					return
				default:
					if index == 0 {
						status = comwrapper.DataBegin
					} else {
						status = comwrapper.DataContinue
					}
					// 创建响应数据切片
					responseData := make([]comwrapper.WrapperData, 0, 3) // 预分配容量为2

					response, err := stream.Recv()
					if err != nil {
						if err == io.EOF {
							wLogger.Infow("WrapperWrite stream ended normally", "sid", inst.sid)
							goto endLoop
						}
						wLogger.Errorw("WrapperWrite read stream error", "error", err, "sid", inst.sid)
						close(done)
						return
					}

					// 处理usage数据
					if response.Usage != nil {
						last = true
						status = comwrapper.DataEnd
						// 创建usage数据结构
						usageData := struct {
							PromptTokens     int `json:"prompt_tokens"`
							CompletionTokens int `json:"completion_tokens"`
							TotalTokens      int `json:"total_tokens"`
							QuestionTokens   int `json:"question_tokens"`
						}{
							PromptTokens:     response.Usage.PromptTokens,
							CompletionTokens: response.Usage.CompletionTokens,
							TotalTokens:      response.Usage.PromptTokens + response.Usage.CompletionTokens,
							QuestionTokens:   0,
						}

						// 序列化usage数据
						usageJSON, err := json.Marshal(usageData)
						if err != nil {
							wLogger.Errorw("WrapperWrite marshal usage error", "error", err, "sid", inst.sid)
							close(done)
							return
						}
						wLogger.Infow("WrapperWrite usage data", "usage", string(usageJSON), "sid", inst.sid)

						usageWrapperData := comwrapper.WrapperData{
							Key:      "usage",
							Data:     usageJSON,
							Desc:     nil,
							Encoding: "utf-8",
							Type:     comwrapper.DataText,
							Status:   status,
						}
						responseData = append(responseData, usageWrapperData)

					}
					// 处理content数据
					if len(response.Choices) > 0 && response.Choices[0].Delta.Content != "" {
						index += 1
						// 构建与Python代码一致的响应格式
						result := map[string]interface{}{
							"choices": []map[string]interface{}{
								{
									"content": response.Choices[0].Delta.Content,
									"index":   index,
									"role":    "assistant",
								},
							},
							"question_type": "",
						}

						data, err := json.Marshal(result)
						if err != nil {
							wLogger.Errorw("WrapperWrite marshal error", "error", err, "sid", inst.sid)
							close(done)
							return
						}

						// 添加content数据
						content := comwrapper.WrapperData{
							Key:      "content",
							Data:     data,
							Desc:     nil,
							Encoding: "utf-8",
							Type:     comwrapper.DataText,
							Status:   status,
						}
						responseData = append(responseData, content)

					}

					// 在decode模式下发送usage数据
					if isDecodeMode && len(responseData) > 0 {
						// ru
						if last && len(responseData) == 1 {
							// 构建与Python代码一致的响应格式
							index += 1
							lastRst := map[string]interface{}{
								"choices": []map[string]interface{}{
									{
										"content": "",
										"index":   index,
										"role":    "assistant",
									},
								},
								"question_type": "",
							}
							data, _ := json.Marshal(lastRst)

							responseData = append(responseData, comwrapper.WrapperData{
								Key:      "content",
								Data:     data,
								Desc:     nil,
								Encoding: "utf-8",
								Type:     comwrapper.DataText,
								Status:   comwrapper.DataEnd,
							})
						}
						if err = inst.callback(inst.usrTag, responseData, nil); err != nil {
							wLogger.Errorw("WrapperWrite usage callback error", "error", err, "sid", inst.sid)
							close(done)
							return
						}
					}
				}
			}

		endLoop:
			if inst.active {
				if !isDecodeMode {
					keepAliveData := comwrapper.WrapperData{
						Key:      "__keepalive_down",
						Data:     []byte{},
						Desc:     nil,
						Encoding: "utf-8",
						Type:     comwrapper.DataText,
						Status:   comwrapper.DataEnd,
					}

					if err := inst.callback(inst.usrTag, []comwrapper.WrapperData{keepAliveData}, nil); err != nil {
						wLogger.Errorw("WrapperWrite final keepalive_down callback error", "error", err, "sid", inst.sid)
					} else {
						wLogger.Infow("WrapperWrite sent keepalive_down end", "sid", inst.sid)
					}
				}

			}

			wLogger.Infow("WrapperWrite sending end signal", "sid", inst.sid)
			close(done)
			inst.stopQ <- true

		}(streamReq, v.Status)
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
		// 过滤掉tool消息
		var filteredMessages []Message
		for _, msg := range sparkMsg.Messages {
			if msg.Role != "tool" {
				filteredMessages = append(filteredMessages, msg)
			}
		}
		return filteredMessages
	}

	// 如果都不是，则作为普通文本处理
	wLogger.Debugw("Using plain text as message", "prompt", prompt)
	return []Message{{
		Role:    "user",
		Content: prompt,
	}}
}

// formatMessages 格式化消息，支持搜索模板
func formatMessages(prompt string, promptSearchTemplate string) []Message {
	messages := parseMessages(prompt)

	// 如果没有搜索模板，直接返回解析后的消息
	if promptSearchTemplate == "" {
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

	// 格式化搜索内容
	var formattedContent []string
	for _, content := range searchContent {
		formattedText := fmt.Sprintf("[webpage %d begin]\n%s%s\n[webpage %d end]",
			content["index"],
			content["docid"],
			content["document"],
			content["index"])
		formattedContent = append(formattedContent, formattedText)
	}

	// 获取当前日期
	now := time.Now()
	weekdays := []string{"一", "二", "三", "四", "五", "六", "日"}
	currentDate := fmt.Sprintf("%d年%02d月%02d日星期%s",
		now.Year(), now.Month(), now.Day(), weekdays[now.Weekday()])

	// 查找最后一个用户消息并更新其内容
	for i := len(messages) - 1; i >= 0; i-- {
		if messages[i].Role == "user" {
			// 应用模板
			messages[i].Content = fmt.Sprintf(promptSearchTemplate,
				strings.Join(formattedContent, "\n"),
				currentDate,
				messages[i].Content)
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
	Role    string      `json:"role"`
	Content interface{} `json:"content"`
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
