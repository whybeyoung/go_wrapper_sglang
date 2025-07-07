FROM artifacts.iflytek.com/docker-private/aipaas/aiges-build:2.9.13 as builder
ADD inference_wrapper/wrapper_sglang_openai/wrapper.go /home/AIGES/src/wrapper/
ADD  vendor/github.com/whybeyoung/go-openai /home/AIGES/src/github.com/whybeyoung/go-openai
ADD /utils /home/AIGES/src/github.com/whybeyoung/go_wrapper_sglang/utils
RUN rm -f /home/AIGES/bin/libwrapper.so
RUN bash /home/AIGES/build.wrapper.sh


FROM artifacts.iflytek.com/docker-private/maas/inference:22.04-cuda124-dev-py310

RUN apt-get update && \
    apt-get install -y --no-install-recommends \
        libnl-3-dev \
        libnl-route-3-dev && \
    apt-get install -y curl && \
    /usr/local/bin/python -m pip install --upgrade pip && \
    pip install sgl-kernel==0.1.0 && \
    pip install  --upgrade mooncake_transfer_engine==0.3.0b6 pyverbs prometheus-client && \
    pip install "sglang[all]==0.4.7" && \
    rm -rf /var/lib/apt/lists/*
#ADD /vendor/github.com/sgl-project/sglang /home/aiges/src/github.com/sgl-project/sglang
#ADD ./engine_start_test.sh /home/aiges
#WORKDIR /home/aiges/src/github.com/sgl-project/sglang
#RUN pip install -e "python[all]"
#ADD sglang/fz_sglang /usr/local/src/sglang
ADD /aiservice_2.9.11.5.bin /home/aiges/
COPY /aiservice_2.9.11.5.bin/lib /home/aiges/library
ADD /scripts/. /home/aiges/
RUN chmod +x /home/aiges/health_check.sh && \
    chmod +x /home/aiges/warmup.py
WORKDIR /home/aiges
COPY --from=builder /home/AIGES/bin/libwrapper.so /home/aiges
COPY --from=builder /home/AIGES/bin/AIservice /home/aiges
RUN chmod 755 /home/aiges/libwrapper.so