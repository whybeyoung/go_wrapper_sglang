package main

import (
	"bufio"
	"comwrapper"
	"context"
	"encoding/json"
	"errors"
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
	sessionManager              *SessionManager
	requestManager              *RequestManager
	httpServerPort              int
	metricsAutoReport           bool               // 添加全局变量存储是否自动上报指标
	isDecodeMode                bool               // 添加全局变量存储PD模式
	promptSearchTemplate        string             // 添加全局变量存储搜索模板
	promptSearchTemplateNoIndex string             // 添加全局变量存储不带索引的搜索模板
	dataChanBufferSize          = 1024 * 64        // 数据通道缓冲区大小，设置为1024以容纳约128KB的数据
	timeDDL                     = 60 * time.Minute // 超时时间
	traceFunc                   func(usrTag string, key string, value string) (code int)
	meterFunc                   func(usrTag string, key string, count int) (code int)
	LbExtraFunc                 func(params map[string]string) error
	throughputMax               float64
)

const (
	ENGINE_TOKEN_LIMIT_EXCEEDED_ERR = "ENGINE_TOKEN_LIMIT_EXCEEDED;code=2001"
	ENGINE_REQUEST_ERR              = "ENGINE_REQUEST_ERR;code=2002"
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

// SingleStrOut 单个请求的输出结构
type SingleStrOut struct {
	FinishedReason   map[string]interface{} `msgpack:"finished_reason"`
	OutputStr        string                 `msgpack:"output_str"`
	OutputID         int                    `msgpack:"output_id"`
	PromptTokens     int                    `msgpack:"prompt_tokens"`
	CompletionTokens int                    `msgpack:"completion_tokens"`
	CachedTokens     int                    `msgpack:"cached_tokens"`
	SpecVerifyCt     int                    `msgpack:"spec_verify_ct"`
	// InputTokenLogprobsVal     float64                `msgpack:"input_token_logprobs_val"`
	// InputTokenLogprobsIdx     int                    `msgpack:"input_token_logprobs_idx"`
	// OutputTokenLogprobsVal    float64                `msgpack:"output_token_logprobs_val"`
	// OutputTokenLogprobsIdx    int                    `msgpack:"output_token_logprobs_idx"`
	// InputTopLogprobsVal       []interface{}          `msgpack:"input_top_logprobs_val"`
	// InputTopLogprobsIdx       []interface{}          `msgpack:"input_top_logprobs_idx"`
	// OutputTopLogprobsVal      []interface{}          `msgpack:"output_top_logprobs_val"`
	// OutputTopLogprobsIdx      []interface{}          `msgpack:"output_top_logprobs_idx"`
	// InputTokenIdsLogprobsVal  []interface{}          `msgpack:"input_token_ids_logprobs_val"`
	// InputTokenIdsLogprobsIdx  []interface{}          `msgpack:"input_token_ids_logprobs_idx"`
	// OutputTokenIdsLogprobsVal []interface{}          `msgpack:"output_token_ids_logprobs_val"`
	// OutputTokenIdsLogprobsIdx []interface{}          `msgpack:"output_token_ids_logprobs_idx"`
	// OutputHiddenStates        []float64              `msgpack:"output_hidden_states"`
}

// wrapperInst 结构体定义
type wrapperInst struct {
	usrTag         string
	sid            string
	client         *OpenAIClient
	firstFrame     bool
	callback       comwrapper.CallBackPtr
	params         map[string]string
	active         bool
	SessionManager *SessionManager
	Stream         *openai.ChatCompletionStream
	// StopStreamChan      chan bool
	SingleOutChan       chan SingleStrOut
	chanMu              sync.Mutex // 添加互斥锁保护 channel 操作
	SingleOutChanClosed bool       // 添加标志位记录 channel 状态
	thinkingMode        bool       // 添加标志位记录 thinking 模式
	jsonMode            bool       // 添加标志位记录 json 模式
	functionCallMode    bool       // 添加标志位记录 function call 模式
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

// todo move to single file
type BatchStrOut struct {
	Rids             []string                 `msgpack:"rids"`
	FinishedReasons  []map[string]interface{} `msgpack:"finished_reasons"`
	OutputStrs       []string                 `msgpack:"output_strs"`
	OutputIds        []int                    `msgpack:"output_ids"`
	PromptTokens     []int                    `msgpack:"prompt_tokens"`
	CompletionTokens []int                    `msgpack:"completion_tokens"`
	CachedTokens     []int                    `msgpack:"cached_tokens"`
	SpecVerifyCt     []int                    `msgpack:"spec_verify_ct"`
	// InputTokenLogprobsVal     []float64                `msgpack:"input_token_logprobs_val"`
	// InputTokenLogprobsIdx     []int                    `msgpack:"input_token_logprobs_idx"`
	// OutputTokenLogprobsVal    []float64                `msgpack:"output_token_logprobs_val"`
	// OutputTokenLogprobsIdx    []int                    `msgpack:"output_token_logprobs_idx"`
	// InputTopLogprobsVal       [][]interface{}          `msgpack:"input_top_logprobs_val"`
	// InputTopLogprobsIdx       [][]interface{}          `msgpack:"input_top_logprobs_idx"`
	// OutputTopLogprobsVal      [][]interface{}          `msgpack:"output_top_logprobs_val"`
	// OutputTopLogprobsIdx      [][]interface{}          `msgpack:"output_top_logprobs_idx"`
	// InputTokenIdsLogprobsVal  [][]interface{}          `msgpack:"input_token_ids_logprobs_val"`
	// InputTokenIdsLogprobsIdx  [][]interface{}          `msgpack:"input_token_ids_logprobs_idx"`
	// OutputTokenIdsLogprobsVal [][]interface{}          `msgpack:"output_token_ids_logprobs_val"`
	// OutputTokenIdsLogprobsIdx [][]interface{}          `msgpack:"output_token_ids_logprobs_idx"`
	// OutputHiddenStates        [][]float64              `msgpack:"output_hidden_states"`
}

// metrics for lb
type ContentItem struct {
	Key   string  `json:"key"`
	Value float64 `json:"value"`
}

type Metric struct {
	Name    string        `json:"name"`
	Type    string        `json:"type"`
	Content []ContentItem `json:"content"`
}

func ParseMetrics(text string) map[string]*Metric {
	lines := strings.Split(text, "\n")
	metrics := make(map[string]*Metric, len(lines))
	var currentMetric *Metric

	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}

		switch {
		case strings.HasPrefix(line, "# HELP"):
			continue

		case strings.HasPrefix(line, "# TYPE"):
			parts := strings.Fields(line)
			if len(parts) < 4 {
				continue
			}

			currentMetric = &Metric{
				Name:    parts[2],
				Type:    parts[3],
				Content: []ContentItem{},
			}
			metrics[parts[2]] = currentMetric

		default:
			if currentMetric == nil {
				continue
			}
			keyValue := strings.SplitN(line, " ", 2)
			if len(keyValue) == 2 {
				val, err := strconv.ParseFloat(keyValue[1], 64)
				if err != nil {
					// 解析失败你可以选择跳过或赋0
					val = 0
				}
				currentMetric.Content = append(currentMetric.Content, ContentItem{
					Key:   keyValue[0],
					Value: val,
				})
			}
		}
	}

	return metrics
}

// SessionManager 管理分词器和ZMQ通信
type SessionManager struct {
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
	Inst         *wrapperInst // 添加 wrapperInst 指针
	CreatedTime  time.Time
	FinishedTime time.Time
	Text         string
}

// NewSessionManager 创建新的SessionManager实例
func NewSessionManager(leaderAddr, zmqPort, pdType string) (*SessionManager, error) {
	context, err := zmq.NewContext()
	if err != nil {
		return nil, fmt.Errorf("failed to create zmq context: %v", err)
	}
	// 设置 PD role，如果未指定则默认为 decode
	if pdType == "" {
		pdType = "decode"
	}

	return &SessionManager{
		zmqContext: context,
		zmqPort:    zmqPort,
		leaderAddr: leaderAddr,
		logger:     zLogger,
		RIDToState: make(map[string]*ReqState),
		ridOpChans: make(map[string]chan func()),
		pdRole:     pdType,
	}, nil
}

// HandleBatchOutput processes batch outputs from the tokenizer
func (sm *SessionManager) HandleBatchOutput(data []byte) error {
	var batchOut BatchStrOut
	if err := msgpack.Unmarshal(data, &batchOut); err != nil {
		return err
	}
	start := time.Now()
	defer sm.logger.Infow("Handle batch output end", "cost", time.Since(start), "rids", strings.Join(batchOut.Rids, "|"))
	// 并发处理每个 rid
	for i, rid := range batchOut.Rids {
		// TO do 这里需要优化，需要根据rid来获取state，而不是根据index 这里需要
		func(idx int, requestID string) {
			// 先检查状态是否存在
			sm.mu.RLock()
			state, exists := sm.RIDToState[requestID]
			sm.mu.RUnlock()

			if !exists {
				sm.logger.Warnw("State not found for rid", "rid", requestID)
				return
			}
			if state == nil {
				sm.logger.Warnw("State nil for rid", "rid", requestID)
				return
			}
			// 检查实例是否还处于活动状态
			if !state.Inst.active {
				sm.logger.Warnw("Instance is not active", "rid", requestID)
				return
			}

			// 构建 SingleStrOut 结构
			singleOut := SingleStrOut{}

			// 安全地设置字段值
			if idx < len(batchOut.FinishedReasons) {
				singleOut.FinishedReason = batchOut.FinishedReasons[idx]
			}
			if idx < len(batchOut.OutputStrs) {
				singleOut.OutputStr = batchOut.OutputStrs[idx]
			}
			if idx < len(batchOut.OutputIds) {
				singleOut.OutputID = batchOut.OutputIds[idx]
			}
			if idx < len(batchOut.PromptTokens) {
				singleOut.PromptTokens = batchOut.PromptTokens[idx]
			}
			if idx < len(batchOut.CompletionTokens) {
				singleOut.CompletionTokens = batchOut.CompletionTokens[idx]
			}
			if idx < len(batchOut.CachedTokens) {
				singleOut.CachedTokens = batchOut.CachedTokens[idx]
			}
			if idx < len(batchOut.SpecVerifyCt) {
				singleOut.SpecVerifyCt = batchOut.SpecVerifyCt[idx]
			}
			// if idx < len(batchOut.InputTokenLogprobsVal) {
			// 	singleOut.InputTokenLogprobsVal = batchOut.InputTokenLogprobsVal[idx]
			// }
			// if idx < len(batchOut.InputTokenLogprobsIdx) {
			// 	singleOut.InputTokenLogprobsIdx = batchOut.InputTokenLogprobsIdx[idx]
			// }
			// if idx < len(batchOut.OutputTokenLogprobsVal) {
			// 	singleOut.OutputTokenLogprobsVal = batchOut.OutputTokenLogprobsVal[idx]
			// }
			// if idx < len(batchOut.OutputTokenLogprobsIdx) {
			// 	singleOut.OutputTokenLogprobsIdx = batchOut.OutputTokenLogprobsIdx[idx]
			// }
			// if idx < len(batchOut.InputTopLogprobsVal) {
			// 	singleOut.InputTopLogprobsVal = batchOut.InputTopLogprobsVal[idx]
			// }
			// if idx < len(batchOut.InputTopLogprobsIdx) {
			// 	singleOut.InputTopLogprobsIdx = batchOut.InputTopLogprobsIdx[idx]
			// }
			// if idx < len(batchOut.OutputTopLogprobsVal) {
			// 	singleOut.OutputTopLogprobsVal = batchOut.OutputTopLogprobsVal[idx]
			// }
			// if idx < len(batchOut.OutputTopLogprobsIdx) {
			// 	singleOut.OutputTopLogprobsIdx = batchOut.OutputTopLogprobsIdx[idx]
			// }
			// if idx < len(batchOut.InputTokenIdsLogprobsVal) {
			// 	singleOut.InputTokenIdsLogprobsVal = batchOut.InputTokenIdsLogprobsVal[idx]
			// }
			// if idx < len(batchOut.InputTokenIdsLogprobsIdx) {
			// 	singleOut.InputTokenIdsLogprobsIdx = batchOut.InputTokenIdsLogprobsIdx[idx]
			// }
			// if idx < len(batchOut.OutputTokenIdsLogprobsVal) {
			// 	singleOut.OutputTokenIdsLogprobsVal = batchOut.OutputTokenIdsLogprobsVal[idx]
			// }
			// if idx < len(batchOut.OutputTokenIdsLogprobsIdx) {
			// 	singleOut.OutputTokenIdsLogprobsIdx = batchOut.OutputTokenIdsLogprobsIdx[idx]
			// }
			// if idx < len(batchOut.OutputHiddenStates) {
			// 	singleOut.OutputHiddenStates = batchOut.OutputHiddenStates[idx]
			// }

			// 发送到实例的通道
			if state.Inst.active {
				select {
				case state.Inst.SingleOutChan <- singleOut:
					// 发送成功
					sm.logger.Debugw("Successfully sent data to SingleOutChan",
						"rid", rid,
						"chanLen", len(state.Inst.SingleOutChan),
						"chanCap", cap(state.Inst.SingleOutChan))
				}
			} else {
				state.Inst.chanMu.Lock()
				if !state.Inst.SingleOutChanClosed {
					close(state.Inst.SingleOutChan)
					state.Inst.SingleOutChanClosed = true
					wLogger.Infow("Instance is not active, close SingleOutChan For rid", "rid", rid)
				}
				state.Inst.chanMu.Unlock()
			}
		}(i, rid)
	}

	return nil
}

// receiveLoop 接收消息的循环
func (sm *SessionManager) receiveLoop() {
	for {
		msg, err := sm.zmqSocket.RecvBytes(0)
		if err != nil {
			sm.logger.Errorw("Receive error:", "error", err)
			continue
		}

		// 使用协程处理消息，避免阻塞接收循环
		func(message []byte) {
			if err := sm.HandleBatchOutput(message); err != nil {
				sm.logger.Errorw("Handle batch output error:", "error", err)
			}
		}(msg)
	}
}

func reportSglangMetrics(serverMetricURL string, isDecodeMode bool) {
	wLogger.Debugw("sglang metric start")
	httpClient := http.Client{
		Timeout: time.Second,
	}
	var metricName, logstr string
	if isDecodeMode {
		metricName = "sglang:gen_throughput"
		logstr = "decode throughput"
	} else {
		metricName = "sglang:prompt_tokens_total"
		logstr = "prefill throughput"
	}
	sleepInterval := 10 * time.Second
	lastReportTime := time.Now()
	lastMetricValue := float64(0)
	newMetricValue := float64(0)
	throughput := float64(0)
	has_generation_tokens_total := false
	generation_token_metric := "sglang:generation_tokens_total"
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
		//wLogger.Debugw("sglang metric", "resp", string(body))
		sglangMetrics := ParseMetrics(string(body))
		if has_generation_tokens_total {
			metricName = generation_token_metric
		}
		if sglangMetrics != nil {
			if metric, ok := sglangMetrics[metricName]; ok {
				lastMetricValue = newMetricValue
				newMetricValue = metric.Content[0].Value
				delta := newMetricValue - lastMetricValue
				deltaTime := time.Since(lastReportTime)
				lastReportTime = time.Now()
				if isDecodeMode {
					throughput = newMetricValue
				} else {
					throughput = float64(delta) / deltaTime.Seconds()
				}

				res := map[string]string{}
				wrapperInfo := map[string]any{}
				wrapperInfo["throughput"] = throughput
				wrapperInfo["throughput_max"] = throughputMax
				wrapperInfoBytes, _ := json.Marshal(wrapperInfo)
				res["wrapper_info"] = string(wrapperInfoBytes)
				wLogger.Debugw(logstr, "throughput", wrapperInfo["throughput"], "throughput_max", wrapperInfo["throughput_max"])
				err = LbExtraFunc(res)
				if err != nil {
					wLogger.Errorw("sglang metric", "err", err.Error())
				}
			} else if _, ok = sglangMetrics[generation_token_metric]; ok && isDecodeMode {
				has_generation_tokens_total = true
			}
		}
		time.Sleep(sleepInterval)
	}
}

// Start 启动ZMQ接收器
func (sm *SessionManager) Start() error {
	socket, err := sm.zmqContext.NewSocket(zmq.PULL)
	if err != nil {
		return fmt.Errorf("failed to create zmq socket: %v", err)
	}
	sm.zmqSocket = socket

	err = socket.Bind(fmt.Sprintf("tcp://*:%s", sm.zmqPort))
	if err != nil {
		return fmt.Errorf("failed to bind zmq socket: %v", err)
	}

	sm.logger.Infow("ZMQReceiver loop started...")
	go sm.receiveLoop()
	return nil
}

// Stop 停止ZMQ接收器
func (sm *SessionManager) Stop() {
	if sm.zmqSocket != nil {
		sm.zmqSocket.Close()
	}
	if sm.zmqContext != nil {
		sm.zmqContext.Term()
	}
}

// AddRequest 添加新的请求
func (sm *SessionManager) AddRequest(rid string, inst *wrapperInst) *ReqState {
	state := &ReqState{
		CreatedTime: time.Now(),
		Inst:        inst, // 保存 wrapperInst 指针
	}

	sm.mu.Lock()
	sm.RIDToState[rid] = state
	sm.mu.Unlock()

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

	// 获取指标上报地址
	if v, ok := cfg["throughput_max"]; ok {
		throughputMax, _ = strconv.ParseFloat(v, 64)
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
		sessionManager, err = NewSessionManager(leaderAddr, zmqPort, pdType)
		if err != nil {
			return fmt.Errorf("failed to create tokenizer manager: %v", err)
		}

		if err := sessionManager.Start(); err != nil {
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
		metricsAutoReport = true
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

	// 启动 metrics 上报
	// sglang指标上报
	if metricsAutoReport {
		go reportSglangMetrics(fmt.Sprintf("http://localhost:%d/metrics", httpServerPort), isDecodeMode)
	}

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
		usrTag:         usrTag,
		sid:            sid,
		client:         requestManager.Client,
		firstFrame:     true,
		callback:       cb,
		params:         params,
		active:         true,
		SessionManager: sessionManager,
		SingleOutChan:  make(chan SingleStrOut, dataChanBufferSize),
		// StopStreamChan:      make(chan bool, 2),
		SingleOutChanClosed: false, // 使用大写开头的字段名
	}
	if inst.SessionManager != nil {
		inst.SessionManager.AddRequest(inst.sid, inst) // 传入 inst 指针
		wLogger.Infow("WrapperCreate added request state for pd mode", "sid", inst.sid)
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

// handleSingleOutChan 处理SingleOutChan的数据
func (inst *wrapperInst) handleRidResponse() {
	// 添加 token 计数变量
	var totalPromptTokens, totalCompletionTokens int
	var chunkIndex int = 0
	var finished bool = false

	defer inst.closeHttpStream()

	for {
		if !inst.active {
			wLogger.Infow("WrapperWrite Recv Goroutine exit", "sid", inst.sid)
			return
		}
		select {
		case singleOut, ok := <-inst.SingleOutChan:
			if !ok {
				wLogger.Infow("SingleOutChan closed, exiting", "sid", inst.sid)
				return
			}
			if chunkIndex == 0 && inst.SessionManager.IsPrefillMode() {
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
					wLogger.Infow("Prefill Get First Chunk, Set inst.active to false, send keepalive_down for pd", "sid", inst.sid)
				}
				// preifll 第一帧就结束 并发送释放信号
				inst.active = false
				return
			}

			// 累加 token 数量
			totalPromptTokens = singleOut.PromptTokens
			totalCompletionTokens = singleOut.CompletionTokens

			var chunkResponse map[string]interface{}
			if singleOut.OutputStr == "</think>" {
				inst.thinkingMode = false
				singleOut.OutputStr = ""
			}
			if singleOut.OutputStr == "<think>" && chunkIndex == 0 {
				inst.thinkingMode = true
				singleOut.OutputStr = ""
			}
			if inst.thinkingMode && !inst.jsonMode {
				// 构建增量响应结构
				chunkResponse = map[string]interface{}{
					"choices": []map[string]interface{}{
						{
							"content":           "",
							"reasoning_content": singleOut.OutputStr,
							"index":             0,
							"role":              "assistant",
						},
					},
					"question_type": "",
				}
			} else if !inst.functionCallMode {
				chunkResponse = map[string]interface{}{
					"choices": []map[string]interface{}{
						{
							"content":           singleOut.OutputStr,
							"reasoning_content": "",
							"index":             0,
							"role":              "assistant",
						},
					},
					"question_type": "",
				}
			} else {
				// 这里需要 tool call parser 流式解析结果
				// 需要重写python这块的 toolcall parser 流式解析结果
				// todo
				chunkResponse = map[string]interface{}{
					"choices": []map[string]interface{}{
						{
							"content":           nil,
							"reasoning_content": "",
							"index":             0,
							"role":              "assistant",
							"function_call":     singleOut.OutputStr,
						},
					},
					"question_type": "",
				}
			}
			chunkIndex++

			// 序列化增量响应
			chunkJSON, err := json.Marshal(chunkResponse)
			if err != nil {
				wLogger.Errorw("Failed to marshal chunk response", "error", err, "sid", inst.sid)
				continue
			}
			// 创建响应数据切片
			responseData := make([]comwrapper.WrapperData, 0, 2)

			// 创建响应数据
			contentData := comwrapper.WrapperData{
				Key:      "content",
				Data:     chunkJSON,
				Desc:     nil,
				Encoding: "utf-8",
				Type:     comwrapper.DataText,
				Status:   comwrapper.DataContinue,
			}

			// 检查是否完成
			if singleOut.FinishedReason != nil {
				contentData.Status = comwrapper.DataEnd

				// 添加累计的 usage 信息
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
					wLogger.Errorw("Failed to marshal usage data", "error", err, "sid", inst.sid)
				} else {
					// 将 usage 数据添加到响应中
					usageWrapperData := comwrapper.WrapperData{
						Key:      "usage",
						Data:     usageJSON,
						Desc:     nil,
						Encoding: "utf-8",
						Type:     comwrapper.DataText,
						Status:   comwrapper.DataEnd,
					}
					responseData = append(responseData, usageWrapperData)
					wLogger.Infow("WrapperWrite  End and send end signal, sending usage data", "sid", inst.sid, "usage", usageJSON)
				}
				// 最后一针需要 清理 inst 和 state
				inst.active = false
				finished = true
			}

			if finished {
				contentData.Status = comwrapper.DataEnd
			}
			responseData = append(responseData, contentData)

			// 发送响应数据
			if err := inst.callback(inst.usrTag, responseData, nil); err != nil {
				wLogger.Errorw("WrapperWrite callback error", "error", err, "sid", inst.sid)
			}
		}
	}

}

func (inst *wrapperInst) closeHttpStream() {
	if inst.Stream != nil {
		inst.Stream.Close()
	}
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

	// 继续处理原有的请求逻辑
	var pdInfo *PDInfo

	if inst.SessionManager.IsPrefillMode() {
		pdInfo = getPDInfo()
		wLogger.Infow("WrapperWrite Prefill generated PD info",
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
		go func() {
			// 按顺序发送所有数据
			if err = inst.callback(inst.usrTag, []comwrapper.WrapperData{kvContent, messagesContent, keepAliveBegin}, nil); err != nil {
				wLogger.Errorw("WrapperWrite callback error", "error", err, "sid", inst.sid)
			}
			wLogger.Infow("WrapperWrite sent kv_info, messages and keepalive_down begin", "sid", inst.sid)
		}()
		// 按顺序发送所有数据
		// time.Sleep(time.Millisecond * 1)

		// if err = inst.callback(inst.usrTag, []comwrapper.WrapperData{kvContent, messagesContent, keepAliveBegin}, nil); err != nil {
		// 	wLogger.Errorw("WrapperWrite callback error", "error", err, "sid", inst.sid)
		// }
		// wLogger.Infow("WrapperWrite sent kv_info, messages and keepalive_down begin", "sid", inst.sid)

	} else {
		// 如果是decode模式，从kv_info中获取PD信息
		for _, v := range req {
			if v.Key == "__kv_info" {
				if err := json.Unmarshal(v.Data, &pdInfo); err != nil {
					wLogger.Errorw("WrapperWrite Decode unmarshal kv_info error", "error", err, "sid", inst.sid)
					return err
				}
				wLogger.Infow("WrapperWrite Decode received PD info from kv_info",
					"sid", inst.sid,
					"bootstrapIP", pdInfo.BootstrapIP,
					"bootstrapPort", pdInfo.BootstrapPort,
					"bootstrapRoom", pdInfo.BootstrapRoom,
					"prefillAddr", pdInfo.PrefillAddr)
				break
			}
		}
	}

	// 启动一个 goroutine 来处理 接收到的SingleOutChan 的数据
	go inst.handleRidResponse()

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
			if responseFormat.Type == "json_schema" || responseFormat.Type == "json_object" {
				inst.jsonMode = true
			}

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

		wLogger.Infow("WrapperWrite request parameters",
			"sid", inst.sid,
			"temperature", temperature,
			"maxTokens", maxTokens)

		// 创建流式请求
		formatResult := formatMessages(string(v.Data), promptSearchTemplate, promptSearchTemplateNoIndex)
		messages := convertToOpenAIMessages(formatResult.Messages)
		functions := formatResult.Functions

		if os.Getenv("IS_REASONING_MODEL") == "true" {
			if len(messages) > 0 {
				lastMsg := messages[len(messages)-1]
				if lastMsg.Role != "assistant" {
					messages = append(messages, openai.ChatCompletionMessage{
						Role:    "assistant",
						Content: "<think>\n",
					})
					inst.thinkingMode = true
				}
			}
		}

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

		if len(functions) > 0 {
			wLogger.Debugw("WrapperWrite functions", "functions", functions)
			inst.functionCallMode = true

			// 将 functions 转换为 openai.Tool
			tools := make([]openai.Tool, len(functions))
			for i, function := range functions {
				tools[i] = openai.Tool{
					Type:     "function",
					Function: function,
				}
			}
			streamReq.Tools = tools
			streamReq.ToolChoice = "auto"
		}

		if responseFormat.Type != "" {
			streamReq.ResponseFormat = &responseFormat
		}

		if enableThinking {
			streamReq.ExtraBody["chat_template_kwargs"] = map[string]interface{}{
				"enable_thinking": enableThinking,
			}
		}

		// 使用协程处理流式请求
		go func(req *openai.ChatCompletionRequest, status comwrapper.DataStatus) {
			wLogger.Infow("WrapperWrite Upstream starting stream inference", "sid", inst.sid)

			ctx, cancel := context.WithTimeout(context.Background(), timeDDL)
			defer cancel()

			stream, streamErr := inst.client.openaiClient.CreateChatCompletionStream(ctx, *req)
			if streamErr != nil {
				// 发送错误响应
				if cbkerr := inst.callback(inst.usrTag, nil, streamErr); cbkerr != nil {
					wLogger.Errorw("WrapperWrite Upstream error callback failed", "error", cbkerr, "sid", inst.sid)
				} else {
					wLogger.Infow("WrapperWrite Upstream error callback success", "sid", inst.sid, "error", streamErr)
				}
				return
			}
			inst.Stream = stream
			// defer inst.Stream.Close()t
			// 这里轮询判断 inst.active , 默认调用下 Stream的 Recv
			for inst.active {

				select {
				case <-ctx.Done():
					wLogger.Infow("Context timeout or cancelled, closing stream", "sid", inst.sid)
					inst.active = false
					err := errors.New(fmt.Sprintf("context timeout or cancelled, sid: %s", inst.sid))
					if err := inst.callback(inst.usrTag, nil, err); err != nil {
						wLogger.Errorw("WrapperWrite timeout callback failed", "error", err, "sid", inst.sid)
					}
					return
				// case <-inst.StopStreamChan:
				// 	wLogger.Infow("StopStreamChan received, closing stream", "sid", inst.sid)
				// 	inst.active = false
				// 	return
				default:
					if _, err := inst.Stream.Recv(); err != nil {
						wLogger.Debugw("Stream Recv error...", "error", err, "sid", inst.sid)
						// 第一帧就报错了，比如是 超长了
						inst.active = false
						// 检查是否是token超限错误
						if isTokenLimitExceededError(err) {
							wLogger.Warnw("Token limit exceeded error detected", "error", err, "sid", inst.sid)
							// 创建一个自定义的token超限错误
							tokenLimitErr := errors.New(ENGINE_TOKEN_LIMIT_EXCEEDED_ERR)
							if err := inst.callback(inst.usrTag, nil, tokenLimitErr); err != nil {
								wLogger.Errorw("Token limit exceeded callback failed", "error", err, "sid", inst.sid)
							}
						} else {
							wLogger.Errorw("Recv error...", "error", err, "sid", inst.sid)
						}
					}
					return
				}
			}
			// 实例已经不活跃了， 退出协程 清理 closeStgream
			wLogger.Infow("Write Upstream Stream stopped", "sid", inst.sid)
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
	wLogger.Debugw("WrapperDestroy", "sid", inst.sid)

	inst.active = false
	if !inst.SingleOutChanClosed {
		inst.SingleOutChanClosed = true
		close(inst.SingleOutChan)
	}
	// 只删除状态，不关闭通道
	if inst.SessionManager != nil {
		inst.SessionManager.DeleteState(inst.sid)
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

// MessageParseResult 消息解析结果
type MessageParseResult struct {
	Messages  []Message                    `json:"messages"`
	Functions []*openai.FunctionDefinition `json:"functions,omitempty"`
}

// parseMessages 解析消息
func parseMessages(prompt string) MessageParseResult {
	// 尝试解析JSON格式的消息
	var messages []Message
	if err := json.Unmarshal([]byte(prompt), &messages); err == nil {
		// 检查消息格式是否正确
		for _, msg := range messages {
			if msg.Role == "" || msg.Content == nil {
				wLogger.Errorw("Invalid message format", "message", msg)
				return MessageParseResult{
					Messages: []Message{{
						Role:    "user",
						Content: prompt,
					}},
					Functions: []*openai.FunctionDefinition{},
				}
			}
		}
		return MessageParseResult{
			Messages:  messages,
			Functions: []*openai.FunctionDefinition{},
		}
	}

	// 如果不是JSON格式，尝试解析为Spark格式
	var sparkMsg struct {
		Messages  []Message                    `json:"messages"`
		Functions []*openai.FunctionDefinition `json:"functions,omitempty"`
	}
	if err := json.Unmarshal([]byte(prompt), &sparkMsg); err == nil {
		// 直接返回所有消息和函数，包括tool消息
		return MessageParseResult{
			Messages:  sparkMsg.Messages,
			Functions: sparkMsg.Functions,
		}
	}

	// 如果都不是，则作为普通文本处理
	wLogger.Debugw("Using plain text as message", "prompt", prompt)
	return MessageParseResult{
		Messages: []Message{{
			Role:    "user",
			Content: prompt,
		}},
		Functions: []*openai.FunctionDefinition{},
	}
}

// formatMessages 格式化消息，支持搜索模板
func formatMessages(prompt string, promptSearchTemplate string, promptSearchTemplateNoIndex string) MessageParseResult {
	parseResult := parseMessages(prompt)
	messages := parseResult.Messages

	// 如果没有搜索模板，直接返回解析后的消息和函数
	if promptSearchTemplate == "" && promptSearchTemplateNoIndex == "" {
		return MessageParseResult{
			Messages:  messages,
			Functions: parseResult.Functions,
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

	// 如果没有tool消息，直接返回
	if lastToolMsg == nil {
		return MessageParseResult{
			Messages:  messages,
			Functions: parseResult.Functions,
		}
	}

	// 解析tool消息内容
	var searchContent []map[string]interface{}
	if err := json.Unmarshal([]byte(lastToolMsg.Content.(string)), &searchContent); err != nil {
		wLogger.Errorw("Failed to parse tool message content", "error", err)
		return MessageParseResult{
			Messages:  messages,
			Functions: parseResult.Functions,
		}
	}

	// 如果没有搜索内容，直接返回
	if len(searchContent) == 0 {
		return MessageParseResult{
			Messages:  messages,
			Functions: parseResult.Functions,
		}
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
				return MessageParseResult{
					Messages:  messages,
					Functions: parseResult.Functions,
				}
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
				return MessageParseResult{
					Messages:  messages,
					Functions: parseResult.Functions,
				}
			}

			messages[i].Content = result.String()
			break
		}
	}

	return MessageParseResult{
		Messages:  messages,
		Functions: parseResult.Functions,
	}
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
	// 添加一个随机因子，使用纳秒时间戳的低32位
	randomFactor := time.Now().UnixNano() & ((1 << 32) - 1)
	offset := (ipHash * (1 << 32)) + (int64(port) * (1 << 16)) + timeBase + randomFactor

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
		Timeout: timeDDL,
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
	Model       string        `json:"model"`
	Messages    []Message     `json:"messages"`
	Temperature float64       `json:"temperature,omitempty"`
	MaxTokens   int           `json:"max_tokens,omitempty"`
	Stream      bool          `json:"stream,omitempty"`
	Tools       []openai.Tool `json:"tools,omitempty"`
	ToolChoice  string        `json:"tool_choice,omitempty"`
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
func (sm *SessionManager) GetRequestState(rid string) (*ReqState, bool) {
	sm.mu.RLock()
	defer sm.mu.RUnlock()
	state, exists := sm.RIDToState[rid]
	return state, exists
}

// DeleteState 安全地删除请求状态
func (sm *SessionManager) DeleteState(rid string) {
	if sm == nil {
		return
	}
	sm.mu.Lock()
	delete(sm.RIDToState, rid)
	sm.mu.Unlock()
}

// IsDecodeMode 判断是否为 decode 模式
func (sm *SessionManager) IsDecodeMode() bool {
	return sm.pdRole == "decode"
}

// IsPrefillMode 判断是否为 prefill 模式
func (sm *SessionManager) IsPrefillMode() bool {
	return sm.pdRole == "prefill"
}

// GetPDRole 获取当前 PD role
func (sm *SessionManager) GetPDRole() string {
	return sm.pdRole
}
