export LD_LIBRARY_PATH=$LD_LIBRARY_PATH:./lib:./
export PRETRAINED_MODEL_NAME=llama2_7b
export INFER_TYPE=base
export TOKENIZER_PATH=/workspace/models/
export FULL_MODEL_PATH=/workspace/models/
export LOCAL_SERVER_ABLE=1
export WIDGET_PLUGIN_MODE=go
export CMD_EXTRA_ARGS="--mem-fraction-static 0.93 --torch-compile-max-bs 8 --max-running-requests 20"
export CUDA_VISIBLE_DEVICES=4
export IS_REASONING_MODEL="false"

export LOADER_RES_GLOBAL_ABLE=0
#export LOADER_RES_GLOBAL_UID=nil
#export LOADER_RES_GLOBAL_APPID=4CC5779A
export LOADER_RES_ATSHOST=http://10.101.3.222:18099
export LOADER_RES_GLOBAL_INDEXHOST=http://10.101.4.224:9005/individuation/atp/search_frame_index
export LOADER_RES_TIMEOUT=20000
export LOADER_RES_INDEXHOST=http://10.101.4.224:9005/individuation/llm/search_frame_index
export LOADER_RES_INDEXHOSTV2=http://10.101.4.224:9005/individuation/atp/search_frame_index



export LOADER_RES_PATCH_NOTIFY=2
export LOADER_RES_PATCH_INDEX_MODE=1

export AIGES_WRAPPER_VERSION=1.0.0
#./AIservice -m=0 -c=aiges.toml -s=mocksvc -u=http://10.1.87.70:6868 -p=guiderAllService -g=gas
#./AIservice -m=1 -c=s6fb9d652.toml -s=xloadlora -u=http://10.101.2.4:6868 -p=cbm -g=hu
./AIservice -m=0 -c=aiges.toml -s=svcName -u=http://companion.xfyun.iflytek:6868 -p=AIaaS -g=dx