# HA-Services-Deployer 容器形态（scratch 单二进制，零外拉）。
# 构建上下文 = 本目录；hasd 由构建步骤静态编译后放入：
#   CGO_ENABLED=0 go build -trimpath -o deploy/docker/hasd .
# 运行期 CWD=/data（卷），config.yaml 与 manifest.json 由该卷提供。
FROM scratch
COPY hasd /hasd
WORKDIR /data
ENTRYPOINT ["/hasd"]
