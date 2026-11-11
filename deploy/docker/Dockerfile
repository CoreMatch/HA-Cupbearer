# HA-Cupbearer 容器形态（scratch 单二进制，零外拉）。
# 构建上下文 = 本目录；hacb 由构建步骤静态编译后放入：
#   CGO_ENABLED=0 go build -trimpath -o deploy/docker/hacb .
# 运行期 CWD=/data（卷），config.yaml 与 manifest.json 由该卷提供。
FROM scratch
COPY hacb /hacb
WORKDIR /data
ENTRYPOINT ["/hacb"]
