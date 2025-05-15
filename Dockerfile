FROM artifacts.iflytek.com/docker-private/aipaas/aiges-build:2.9.11.6 as builder
ADD inference_wrapper/wrapper_sglang_openai/wrapper.go /home/AIGES/src/wrapper/
ADD  vendor/github.com/whybeyoung/go-openai /home/AIGES/src/github.com/whybeyoung/go-openai
RUN rm -f /home/AIGES/bin/libwrapper.so
RUN bash /home/AIGES/build.wrapper.sh



FROM artifacts.iflytek.com/docker-private/maas/sglang-wrapper-moon:1.0.75

RUN pip install sgl-kernel==0.1.0
RUN pip install  --upgrade mooncake_transfer_engine==0.3.0b6 pyverbs prometheus-client
ADD /vendor/github.com/sgl-project/sglang /home/aiges/src/github.com/sgl-project/sglang
ADD ./engine_start_test.sh /home/aiges
WORKDIR /home/aiges/src/github.com/sgl-project/sglang
RUN pip install -e "python[all]"
#RUN pip install sglang==0.4.6.post3
#ADD sglang/fz_sglang /usr/local/src/sglang
WORKDIR /home/aiges
COPY --from=builder /home/AIGES/bin/libwrapper.so /home/aiges
COPY --from=builder /home/AIGES/bin/AIservice /home/aiges
RUN chmod 755 /home/aiges/libwrapper.so