package main

import (
	"bufio"
	"comwrapper"
	"context"
	"encoding/json"
	"fmt"
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
	zmq "github.com/pebbe/zmq4"
	"github.com/vmihailenco/msgpack/v5"
	"github.com/whybeyoung/go-openai"
)

var (
	wLogger *utils.Logger
	zLogger *utils.Logger

	logLevel                    = "debug"
	logCount                    = 10
	logSize                     = 100
	logAsync                    = true
	logPath                     = "/log/app/wrapper.log"
	zmqLogPath                  = "/log/app/zmqReceiver.log"
	respKey                     = "content"
	requestManager              *RequestManager
	tokenizerManager            *TokenizerManager
	httpServerPort              int
	isDecodeMode                bool   // 添加全局变量存储PD模式
	promptSearchTemplate        string // 添加全局变量存储搜索模板
	promptSearchTemplateNoIndex string // 添加全局变量存储不带索引的搜索模板
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

// todo move to single file
type BatchStrOut struct {
	Rids                      []string                 `msgpack:"rids"`
	FinishedReasons           []map[string]interface{} `msgpack:"finished_reasons"`
	OutputStrs                []string                 `msgpack:"output_strs"`
	OutputIds                 []int                    `msgpack:"output_ids"`
	PromptTokens              []int                    `msgpack:"prompt_tokens"`
	CompletionTokens          []int                    `msgpack:"completion_tokens"`
	CachedTokens              []int                    `msgpack:"cached_tokens"`
	SpecVerifyCt              []int                    `msgpack:"spec_verify_ct"`
	InputTokenLogprobsVal     []float64                `msgpack:"input_token_logprobs_val"`
	InputTokenLogprobsIdx     []int                    `msgpack:"input_token_logprobs_idx"`
	OutputTokenLogprobsVal    []float64                `msgpack:"output_token_logprobs_val"`
	OutputTokenLogprobsIdx    []int                    `msgpack:"output_token_logprobs_idx"`
	InputTopLogprobsVal       [][]interface{}          `msgpack:"input_top_logprobs_val"`
	InputTopLogprobsIdx       [][]interface{}          `msgpack:"input_top_logprobs_idx"`
	OutputTopLogprobsVal      [][]interface{}          `msgpack:"output_top_logprobs_val"`
	OutputTopLogprobsIdx      [][]interface{}          `msgpack:"output_top_logprobs_idx"`
	InputTokenIdsLogprobsVal  [][]interface{}          `msgpack:"input_token_ids_logprobs_val"`
	InputTokenIdsLogprobsIdx  [][]interface{}          `msgpack:"input_token_ids_logprobs_idx"`
	OutputTokenIdsLogprobsVal [][]interface{}          `msgpack:"output_token_ids_logprobs_val"`
	OutputTokenIdsLogprobsIdx [][]interface{}          `msgpack:"output_token_ids_logprobs_idx"`
	OutputHiddenStates        [][]float64              `msgpack:"output_hidden_states"`
}

// TokenizerManager 管理分词器和ZMQ通信
type TokenizerManager struct {
	zmqContext *zmq.Context
	zmqSocket  *zmq.Socket
	zmqPort    string
	leaderAddr string
	logger     *utils.Logger
	mu         sync.RWMutex
	RIDToState map[string]*ReqState
	// 添加 rid 操作通道映射
	ridOpChans map[string]chan func()
	chanMu     sync.RWMutex
	pdRole     string // 添加 PD role 字段
}

// ReqState 存储每个请求的状态
type ReqState struct {
	OutList      []map[string]interface{}
	OutStrList   [][]byte // 存储每次的文本增量
	Finished     bool
	Event        chan struct{}
	Obj          interface{}
	CreatedTime  time.Time
	FinishedTime time.Time
	Text         string
}

// NewTokenizerManager 创建新的TokenizerManager实例
func NewTokenizerManager(leaderAddr, zmqPort, pdType string) (*TokenizerManager, error) {
	context, err := zmq.NewContext()
	if err != nil {
		return nil, fmt.Errorf("failed to create zmq context: %v", err)
	}

	// 设置 PD role，如果未指定则默认为 decode
	if pdType == "" {
		pdType = "decode"
	}

	return &TokenizerManager{
		zmqContext: context,
		zmqPort:    zmqPort,
		leaderAddr: leaderAddr,
		logger:     zLogger,
		RIDToState: make(map[string]*ReqState),
		ridOpChans: make(map[string]chan func()),
		pdRole:     pdType,
	}, nil
}

// getOrCreateRIDOpChan 获取或创建 rid 的操作通道
func (tm *TokenizerManager) getOrCreateRIDOpChan(rid string) chan func() {
	tm.chanMu.Lock()
	defer tm.chanMu.Unlock()

	ch, exists := tm.ridOpChans[rid]
	if !exists {
		// 创建带缓冲的通道，缓冲区大小为100
		ch = make(chan func(), 4096)
		tm.ridOpChans[rid] = ch
		// 启动操作处理协程
		go tm.processRIDOps(rid, ch)
	}
	return ch
}

// processRIDOps 处理特定 rid 的操作
func (tm *TokenizerManager) processRIDOps(rid string, ch chan func()) {
	for {
		select {
		case op, ok := <-ch:
			if !ok {
				// channel 已关闭，退出处理
				tm.logger.Infow("Operation channel closed, exiting processRIDOps", "rid", rid)
				return
			}
			// 执行操作
			op()
		}
	}
}

// HandleBatchOutput processes batch outputs from the tokenizer
func (tm *TokenizerManager) HandleBatchOutput(data []byte) error {
	var batchOut BatchStrOut
	if err := msgpack.Unmarshal(data, &batchOut); err != nil {
		return err
	}
	tm.logger.Infow("Handle batch output", "rids", strings.Join(batchOut.Rids, "|"))
	// 并发处理每个 rid，不需要等待完成
	for i, rid := range batchOut.Rids {
		go func(idx int, requestID string) {
			// 获取 rid 的操作通道
			opChan := tm.getOrCreateRIDOpChan(requestID)

			// 使用 select 和 ok 模式检查通道状态
			select {
			case opChan <- func() {
				tm.mu.RLock()
				state, exists := tm.RIDToState[requestID]
				tm.mu.RUnlock()

				if !exists {
					tm.logger.Warnw("Received output for rid but state was deleted", "rid", requestID)
					return
				}

				// Build meta_info
				metaInfo := map[string]interface{}{
					"id":            requestID,
					"finish_reason": batchOut.FinishedReasons[idx],
					"prompt_tokens": batchOut.PromptTokens[idx],
				}

				// Add completion and cached tokens
				metaInfo["completion_tokens"] = batchOut.CompletionTokens[idx]
				metaInfo["cached_tokens"] = batchOut.CachedTokens[idx]

				// Process output
				state.Text += batchOut.OutputStrs[idx]

				// 构建增量响应结构
				chunkResponse := map[string]interface{}{
					"choices": []map[string]interface{}{
						{
							"content":           batchOut.OutputStrs[idx],
							"reasoning_content": "",
							"index":             idx,
							"role":              "assistant",
						},
					},
					"question_type": "",
				}

				// 序列化增量响应
				chunkJSON, err := json.Marshal(chunkResponse)
				if err != nil {
					tm.logger.Errorw("Failed to marshal chunk response", "error", err, "rid", requestID)
					return
				}

				state.OutStrList = append(state.OutStrList, chunkJSON)

				outDict := map[string]interface{}{
					"meta_info": metaInfo,
				}

				// Update state
				state.Finished = batchOut.FinishedReasons[idx] != nil
				if state.Finished {
					state.FinishedTime = time.Now()
					metaInfo["e2e_latency"] = state.FinishedTime.Sub(state.CreatedTime).Seconds()

					// 使用 DeleteRequestState 来确保状态和通道都被正确清理
					tm.DeleteRequestState(requestID)
				}

				state.OutList = append(state.OutList, outDict)
				select {
				case state.Event <- struct{}{}:
				default:
					tm.logger.Warnw("Event channel is full or closed", "rid", requestID)
				}
			}:
				// 成功放入操作通道
			default:
				tm.logger.Warnw("Operation channel is full or closed for rid", "rid", requestID)
			}
		}(i, rid)
	}

	return nil
}

// receiveLoop 接收消息的循环
func (tm *TokenizerManager) receiveLoop() {
	for {
		msg, err := tm.zmqSocket.RecvBytes(0)
		if err != nil {
			tm.logger.Errorw("Receive error:", "error", err)
			continue
		}

		// 使用协程处理消息，避免阻塞接收循环
		go func(message []byte) {
			if err := tm.HandleBatchOutput(message); err != nil {
				tm.logger.Errorw("Handle batch output error:", "error", err)
			}
		}(msg)
	}
}

// Start 启动ZMQ接收器
func (tm *TokenizerManager) Start() error {
	socket, err := tm.zmqContext.NewSocket(zmq.PULL)
	if err != nil {
		return fmt.Errorf("failed to create zmq socket: %v", err)
	}
	tm.zmqSocket = socket

	err = socket.Bind(fmt.Sprintf("tcp://*:%s", tm.zmqPort))
	if err != nil {
		return fmt.Errorf("failed to bind zmq socket: %v", err)
	}

	tm.logger.Infow("ZMQReceiver loop started...")
	go tm.receiveLoop()
	return nil
}

// Stop 停止ZMQ接收器
func (tm *TokenizerManager) Stop() {
	if tm.zmqSocket != nil {
		tm.zmqSocket.Close()
	}
	if tm.zmqContext != nil {
		tm.zmqContext.Term()
	}
}

// AddRequest 添加新的请求
func (tm *TokenizerManager) AddRequest(rid string) *ReqState {
	state := &ReqState{
		CreatedTime: time.Now(),
		Event:       make(chan struct{}, 1),
	}

	tm.mu.Lock()
	tm.RIDToState[rid] = state
	tm.mu.Unlock()

	return state
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
	zLogger, err = utils.NewLocalLog(
		utils.SetAsync(logAsync),
		utils.SetLevel(logLevel),
		utils.SetFileName(zmqLogPath),
		utils.SetMaxSize(logSize),
		utils.SetMaxBackups(logCount),
	)
	if err != nil {
		zLogger.Errorw("Failed to create zmq logger", "error", err)
		return
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

	// 获取端口配置
	zmqPort := os.Getenv("ZMQ_SERVER_PORT")
	if zmqPort == "" {
		zmqPort = "10110"
	}

	// 检查PD模式
	pdType := os.Getenv("AIGES_PD_ROLE")

	leaderAddr := os.Getenv("LWS_LEADER_ADDRESS")
	if leaderAddr != "" {
		fmt.Printf("Start ZMQReceiver on %s:%s\n", leaderAddr, zmqPort)
		var err error
		tokenizerManager, err = NewTokenizerManager(leaderAddr, zmqPort, pdType)
		if err != nil {
			return fmt.Errorf("failed to create tokenizer manager: %v", err)
		}

		if err := tokenizerManager.Start(); err != nil {
			return fmt.Errorf("failed to start tokenizer manager: %v", err)
		}
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
		if leaderAddr != "" {

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

	return nil
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
		usrTag:           usrTag,
		sid:              sid,
		client:           requestManager.Client,
		stopQ:            make(chan bool, 2),
		firstFrame:       true,
		callback:         cb,
		params:           params,
		active:           true,
		tokenizerManager: tokenizerManager,
	}
	if inst.tokenizerManager != nil {
		inst.tokenizerManager.AddRequest(inst.sid)
		wLogger.Infow("WrapperCreate added request state for decode mode", "sid", inst.sid)
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
	usrTag           string
	sid              string
	client           *OpenAIClient
	stopQ            chan bool
	firstFrame       bool
	callback         comwrapper.CallBackPtr
	params           map[string]string
	active           bool
	tokenizerManager *TokenizerManager
	stream           *openai.ChatCompletionStream
}

// SafeCloseStream 安全地关闭 stream
func (inst *wrapperInst) SafeCloseStream() {
	if inst.stream == nil {
		return
	}
	func() {
		defer func() {
			if r := recover(); r != nil {
				wLogger.Warnw("Stream already closed", "error", r, "sid", inst.sid)
			}
		}()
		inst.stream.Close()
	}()
	inst.stream = nil
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
		messages := convertToOpenAIMessages(formatMessages(string(v.Data), promptSearchTemplate, promptSearchTemplateNoIndex))

		// thinking := false
		// if os.Getenv("IS_REASONING_MODEL") == "true" {
		//      if len(messages) > 0 {
		//              lastMsg := messages[len(messages)-1]
		//              if lastMsg.Role != "assistant" {
		//                      messages = append(messages, openai.ChatCompletionMessage{
		//                              Role:    "assistant",
		//                              Content: "<think>\n",
		//                      })
		//                      thinking = true
		//              }
		//      }
		// }

		wLogger.Infow("Wrapper Send Engine messages", "messages", messages)

		// 获取enable_thinking参数 这里是针对 qwenv3 的逻辑
		enableThinking := false
		if enableThinkingStr, ok := inst.params["enable_thinking"]; ok {
			enableThinking = strings.ToLower(enableThinkingStr) == "true"
		}

		streamReq := &openai.ChatCompletionRequest{
			Model:       "default",
			Messages:    messages,
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
				"rid":            inst.sid,
				"session_params": map[string]interface{}{
					"rid": inst.sid,
				},
			},
		}
		if enableThinking {
			streamReq.ExtraBody["chat_template_kwargs"] = map[string]interface{}{
				"enable_thinking": enableThinking,
			}
		}

		// 使用协程处理流式请求
		go func(req *openai.ChatCompletionRequest, status comwrapper.DataStatus) {
			wLogger.Infow("WrapperWrite starting stream inference", "sid", inst.sid)

			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
			defer cancel()

			index := 0

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

			// 获取请求状态
			state, exists := inst.tokenizerManager.GetRequestState(inst.sid)
			if !exists {
				wLogger.Warnw("State not found for sid", "sid", inst.sid)
				return
			}

			var totalPromptTokens, totalCompletionTokens int

			for {
				select {
				case <-ctx.Done():
					if ctx.Err() == context.DeadlineExceeded {
						wLogger.Warnw("WrapperWrite stream timeout", "sid", inst.sid)
						goto endLoop
					}
					return
				case <-state.Event:
					// 使用 goroutine 调用 Recv，并处理异常
					go func() {
						defer func() {
							if r := recover(); r != nil {
								wLogger.Warnw("Stream Recv  Closed", "error", r, "sid", inst.sid)
							}
						}()
						stream.Recv() // 不使用 http的响应 ，使用来自 zmq go callback
					}()
					// 获取最新的响应
					if len(state.OutList) > 0 {
						if inst.tokenizerManager.IsPrefillMode() {
							goto endLoop
						}
						if index == 0 && inst.tokenizerManager.IsDecodeMode() {
							wLogger.Infow("Decode mode Get First chunk ", "sid", inst.sid)
						}

						index++
						lastOut := state.OutList[len(state.OutList)-1]
						lastOutStr := state.OutStrList[len(state.OutStrList)-1]
						// 从 meta_info 中获取 token 信息
						if metaInfo, ok := lastOut["meta_info"].(map[string]interface{}); ok {
							if promptTokens, ok := metaInfo["prompt_tokens"].(int); ok {
								totalPromptTokens = promptTokens
							}
							if completionTokens, ok := metaInfo["completion_tokens"].(int); ok {
								totalCompletionTokens = completionTokens
							}
						}

						// 创建响应数据切片
						responseData := make([]comwrapper.WrapperData, 0, 2)

						// 添加 content 响应
						contentData := comwrapper.WrapperData{
							Key:      "content",
							Data:     lastOutStr, // lastOutStr 已经是 []byte 类型
							Desc:     nil,
							Encoding: "utf-8",
							Type:     comwrapper.DataText,
							Status:   comwrapper.DataContinue,
						}

						if state.Finished {
							contentData.Status = comwrapper.DataEnd
						}

						responseData = append(responseData, contentData)

						// 如果请求已完成，添加 usage 信息
						if state.Finished {
							usageData := struct {
								PromptTokens     int `json:"prompt_tokens"`
								CompletionTokens int `json:"completion_tokens"`
								TotalTokens      int `json:"total_tokens"`
								QuestionTokens   int `json:"question_tokens"`
							}{
								PromptTokens:     totalPromptTokens,
								CompletionTokens: totalCompletionTokens,
								TotalTokens:      totalPromptTokens + totalCompletionTokens,
								QuestionTokens:   0,
							}

							usageJSON, err := json.Marshal(usageData)
							if err != nil {
								wLogger.Errorw("WrapperWrite marshal usage error", "error", err, "sid", inst.sid)
							} else {
								usageWrapperData := comwrapper.WrapperData{
									Key:      "usage",
									Data:     usageJSON,
									Desc:     nil,
									Encoding: "utf-8",
									Type:     comwrapper.DataText,
									Status:   comwrapper.DataEnd,
								}
								responseData = append(responseData, usageWrapperData)
							}
						}

						if err := inst.callback(inst.usrTag, responseData, nil); err != nil {
							wLogger.Errorw("WrapperWrite callback error", "error", err, "sid", inst.sid)
						}

						// 如果请求已完成，退出循环
						if state.Finished {
							// 检查并关闭 stream
							inst.SafeCloseStream()
							goto endLoop
						}
					}
				case <-inst.stopQ:
					// 检查并关闭 stream
					inst.SafeCloseStream()
					goto endLoop
				}
			}

		endLoop:
			// 确保 stream 被关闭
			inst.SafeCloseStream()

			if inst.active {
				if inst.tokenizerManager.IsPrefillMode() {
					keepAliveData := comwrapper.WrapperData{
						Key:      "__keepalive_down",
						Data:     []byte{},
						Desc:     nil,
						Encoding: "utf-8",
						Type:     comwrapper.DataText,
						Status:   comwrapper.DataEnd,
					}

					if err := inst.callback(inst.usrTag, []comwrapper.WrapperData{keepAliveData}, nil); err != nil {
						wLogger.Errorw("Prefill Mode  get first chunk ,send keepalive_down callback error", "error", err, "sid", inst.sid)
					} else {
						wLogger.Infow("Prefill Mode get first chunk, sent keepalive_down signal", "sid", inst.sid)
					}
				}
			}

			wLogger.Infow("WrapperWrite End and send end signal", "sid", inst.sid)
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

	// 删除对应的状态
	if inst.tokenizerManager != nil {
		inst.tokenizerManager.DeleteRequestState(inst.sid)
		wLogger.Infow("WrapperDestroy deleted state", "sid", inst.sid)
	}

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

	// 获取当前日期
	now := time.Now()
	weekdays := []string{"日", "一", "二", "三", "四", "五", "六"}
	currentDate := fmt.Sprintf("%d年%02d月%02d日星期%s",
		now.Year(), now.Month(), now.Day(), weekdays[now.Weekday()])

	// 查找最后一个用户消息并更新其内容
	for i := len(messages) - 1; i >= 0; i-- {
		if messages[i].Role == "user" {
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
	TimeBase  int64 // 添加时间基准
	Lock      sync.Mutex
}

// NewCircularIDAllocator 创建ID分配器
func NewCircularIDAllocator(mockIP string, port int) *CircularIDAllocator {
	ip := mockIP
	if ip == "" {
		ip = getLocalIP()
	}

	ipHash := hashIP(ip)
	// 使用纳秒级时间戳作为时间因子
	timeBase := time.Now().UnixNano()
	// 将时间戳右移20位，保留高位，避免ID增长过快
	timeBase = timeBase >> 20
	offset := (ipHash * (1 << 32)) + (int64(port) * (1 << 16)) + timeBase

	return &CircularIDAllocator{
		MinID:     0,
		MaxID:     math.MaxInt64,
		CurrentID: 0,
		IP:        ip,
		Port:      port,
		IPHash:    ipHash,
		Offset:    offset,
		TimeBase:  timeBase,
	}
}

// Allocate 分配新ID
func (a *CircularIDAllocator) Allocate() int64 {
	a.Lock.Lock()
	defer a.Lock.Unlock()

	// 获取当前时间戳并右移20位
	currentTime := time.Now().UnixNano() >> 20
	// 如果时间基准发生变化，更新Offset
	if currentTime != a.TimeBase {
		a.TimeBase = currentTime
		a.Offset = (a.IPHash * (1 << 32)) + (int64(a.Port) * (1 << 16)) + currentTime
	}

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
		// 获取退出码
		if exitErr, ok := err.(*exec.ExitError); ok {
			exitCode := exitErr.ExitCode()
			wLogger.Errorw("Sglang process exited with error",
				"error", err,
				"exit_code", exitCode)
			// 根据退出码进行相应处理
			switch exitCode {
			case 0:
				wLogger.Infow("Sglang process exited normally")
			case 1:
				wLogger.Errorw("Sglang process failed with general error")
			case 2:
				wLogger.Errorw("Sglang process failed with configuration error")
			default:
				wLogger.Errorw("Sglang process failed with unknown error code")
			}
		} else {
			wLogger.Errorw("Sglang process failed to start or was terminated", "error", err)
		}
		// 子进程退出，主进程也退出
		wLogger.Errorw("Sglang process exited, exiting main process")
		os.Exit(1)
	} else {
		wLogger.Debugw("Sglang process exited normally")
		// 子进程正常退出，主进程也退出
		wLogger.Infow("Sglang process exited normally, exiting main process")
		os.Exit(0)
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

// GetRequestState 安全地获取请求状态
func (tm *TokenizerManager) GetRequestState(rid string) (*ReqState, bool) {
	tm.mu.RLock()
	defer tm.mu.RUnlock()
	state, exists := tm.RIDToState[rid]
	return state, exists
}

// DeleteRequestState 安全地删除请求状态
func (tm *TokenizerManager) DeleteRequestState(rid string) {
	tm.mu.Lock()
	delete(tm.RIDToState, rid)
	tm.mu.Unlock()

	// 同时关闭对应的操作通道
	tm.chanMu.Lock()
	if ch, exists := tm.ridOpChans[rid]; exists {
		close(ch)
		delete(tm.ridOpChans, rid)
		tm.logger.Infow("Closed and deleted operation channel", "rid", rid)
	}
	tm.chanMu.Unlock()
}

// IsDecodeMode 判断是否为 decode 模式
func (tm *TokenizerManager) IsDecodeMode() bool {
	return tm.pdRole == "decode"
}

// IsPrefillMode 判断是否为 prefill 模式
func (tm *TokenizerManager) IsPrefillMode() bool {
	return tm.pdRole == "prefill"
}

// GetPDRole 获取当前 PD role
func (tm *TokenizerManager) GetPDRole() string {
	return tm.pdRole
}
