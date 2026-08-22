# 生产镜像：把服务编译成一个静态二进制，跑在 distroless 上。
# 模板和静态资源由 go:embed 编进二进制，所以最终镜像里除了它什么都不需要。

FROM golang:1.25.4-alpine AS build

WORKDIR /src

# 先只拷依赖清单：go.mod/go.sum 没变时这一层命中缓存，不用重新下载模块。
COPY go.mod go.sum ./
RUN go mod download

COPY . .

# CGO_ENABLED=0 —— 数据库驱动是 modernc.org/sqlite,纯 Go 实现，不需要 cgo。
#   这正是当初选它而不是 mattn/go-sqlite3 的价值兑现的地方：能出静态二进制，
#   于是运行镜像可以是 distroless（没有 libc、没有 shell）。
# -trimpath 去掉构建机的绝对路径，-s -w 去掉符号表和调试信息。
RUN CGO_ENABLED=0 GOOS=linux go build \
      -trimpath -ldflags='-s -w' \
      -o /out/server ./cmd/server

# 数据目录在这里建好并交给 nonroot(65532)。
# 正常情况下 /data 会被 PVC 盖掉，这一步是为了「不挂卷也能跑」——
# 本地 docker run 验证镜像时不用额外准备什么。
RUN mkdir -p /out/data && chown -R 65532:65532 /out/data


# static-debian12 里没有 shell、没有包管理器、没有 libc,只有 ca-certificates
# （访问 OpenAI 和 Google API 要用）和时区之外的最小根文件系统。
# 攻击面小到几乎没有：即使应用被 RCE,容器里也没有可以调用的程序。
FROM gcr.io/distroless/static-debian12:nonroot

COPY --from=build /out/server /server
COPY --from=build --chown=nonroot:nonroot /out/data /data

# 时区：代码里大量使用 time.Local（日程按本地时间排布）。
# distroless 不带 /usr/share/zoneinfo,所以 main.go 里 import 了 time/tzdata
# 把时区库编进二进制 —— 这个 TZ 才有东西可查。
ENV TZ=Asia/Shanghai

# nonroot 用户，UID 65532。不是 root 意味着即使容器被攻破，也拿不到
# 挂载卷之外的任何写权限。
USER nonroot:nonroot

EXPOSE 8080

# 数据库落在 /data —— 由 PVC 提供持久化，Pod 重建后数据还在。
# 不写 /app/data 这类路径是为了让挂载点显眼：凡是 /data 之外的都是易失的。
ENTRYPOINT ["/server"]
CMD ["-addr", ":8080", "-db", "/data/app.db"]
