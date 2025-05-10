package comwrapper

type DataType int
type DataStatus int
type CallBackPtr func(usrTag string, respData []WrapperData, err error) (ret error)
type CustomFuncType int

const (
	DataText  DataType = 0 // �~V~G�~\��~U��~M�
	DataAudio DataType = 1 // �~_��~Q�~U��~M�
	DataImage DataType = 2 // �~[��~C~O�~U��~M�
	DataVideo DataType = 3 // �~F�~Q�~U��~M�
	DataPer   DataType = 4 // 个�~@��~L~V�~U��~M�

	DataBegin    DataStatus = 0 // �~V�~U��~M�
	DataContinue DataStatus = 1 // 中�~W��~U��~M�
	DataEnd      DataStatus = 2 // 尾�~U��~M�
	DataOnce     DataStatus = 3 // �~]~^�~Z�~]�~M~U次�~S�~E�
)

type WrapperData struct {
	Key      string            // �~U��~M��| ~G�~F
	Data     []byte            // �~U��~M��~^�~S
	Desc     map[string]string // �~U��~M��~O~O述
	Encoding string            // �~U��~M��~V�| ~A
	Type     DataType          // �~U��~M�类�~^~K
	Status   DataStatus        // �~U��~M��~J��~@~A
}

const (
	FuncTraceLog CustomFuncType = 0
	FuncMeter    CustomFuncType = 1
)
