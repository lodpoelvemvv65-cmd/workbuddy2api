# syntax=docker/dockerfile:1
FROM golang:1.26-alpine AS build
# 国内网络直连 proxy.golang.org 会 i/o timeout，默认切 goproxy.cn。
# 需要换源用 --build-arg GOPROXY=<...> 或 compose 的 build.args 覆盖。
ARG GOPROXY=https://goproxy.cn,direct
ENV GOPROXY=${GOPROXY}
WORKDIR /src
COPY go.mod ./
RUN go mod download
COPY . .
# 一次编译全部二进制（工具进镜像，容器内可直接跑脚本）。全部 -trimpath -s -w。
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/wb2api ./cmd/server \
 && CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/signin_bin ./cmd/signin \
 && CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/login ./cmd/login \
 && CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/credit ./cmd/credit \
 && CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/trial_bin ./cmd/trial \
 && CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/activity_bin ./cmd/activity \
 && CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/dashboard ./cmd/dashboard

FROM alpine:3.20
# apk 默认源 dl-cdn.alpinelinux.org 在国内常超时，实测华为云最快（阿里云索引要 14s+，
# 华为云/腾讯云约 1s）；需要换源用 --build-arg APK_MIRROR=mirrors.cloud.tencent.com 覆盖。
ARG APK_MIRROR=mirrors.huaweicloud.com
# python3：login.sh 的 JSON 解析 / 签到 / 落盘；bash：shell 脚本体。
RUN sed -i "s|dl-cdn.alpinelinux.org|${APK_MIRROR}|g" /etc/apk/repositories \
 && apk add --no-cache wget ca-certificates tzdata python3 bash \
 && adduser -D -u 10001 app \
 && mkdir -p /app/auths /app/data \
 && chown -R app:app /app
WORKDIR /app
# 脚本置入 + 去 CRLF（Windows 检出可能性）在切到 app 之前以 root 完成——
# app 对 root 所有文件无写权限，sed -i 需要写权限。
COPY --from=build /out/wb2api /app/wb2api
COPY --from=build /out/signin_bin /app/signin_bin
COPY --from=build /out/login /app/login
COPY --from=build /out/credit /app/credit
COPY --from=build /out/trial_bin /app/trial_bin
COPY --from=build /out/activity_bin /app/activity_bin
# 独立可视化面板（可选；docker-compose 的 dashboard 服务用它，静态页已 go:embed 进二进制）
COPY --from=build /out/dashboard /app/dashboard
COPY login.sh signin.sh credit.sh trial.sh /app/
# 国际版注册地区自动完善模块（login.sh global 分支 import；scripts/ 无测试/缓存）
COPY scripts/global_region.py /app/scripts/global_region.py
COPY scripts/task_common.py /app/scripts/task_common.py
COPY scripts/task_runner.py /app/scripts/task_runner.py
COPY scripts/school_open_day_2026.py /app/scripts/school_open_day_2026.py
RUN sed -i 's/\r$//' /app/*.sh && chmod 755 /app/*.sh
RUN sed -i 's/\r$//' /app/scripts/*.py && chmod 755 /app/scripts/*.py
# 镜像不带真实配置：落 example 作为默认（生产由挂载卷 /app/config.json 覆盖）
COPY config.example.json /app/config.json
USER app
EXPOSE 7863
HEALTHCHECK --interval=30s --timeout=5s --start-period=5s \
  CMD wget -qO- http://127.0.0.1:7863/healthz || exit 1
ENTRYPOINT ["/app/wb2api", "-config", "/app/config.json"]
