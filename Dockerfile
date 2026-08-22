# 生产镜像：把服务编译成一个静态二进制，跑在 distroless 上。
# 模板和静态资源由 go:embed 编进二进制，所以最终镜像里除了它什么都不需要。

FROM golang:1.25.4-alpine AS build

WORKDIR /src

# 先只拷依赖清单：go.mod/go.sum 没变时这一层命中缓存，不用重新下载模块。
COPY go.mod go.sum ./
RUN go mod download

COPY . .

# CGO_ENABLED=0 —— 两个数据库驱动都是纯 Go 的：本地用的 modernc.org/sqlite,
#   线上用的 go-sql-driver/mysql,都不需要 cgo。于是能出静态二进制，
#   运行镜像可以是 distroless（没有 libc、没有 shell）。
# -trimpath 去掉构建机的绝对路径，-s -w 去掉符号表和调试信息。
RUN CGO_ENABLED=0 GOOS=linux go build \
      -trimpath -ldflags='-s -w' \
      -o /out/server ./cmd/server


# static-debian12 里没有 shell、没有包管理器、没有 libc,只有 ca-certificates
# （访问 OpenAI 和 Google API 要用）和时区之外的最小根文件系统。
# 攻击面小到几乎没有：即使应用被 RCE,容器里也没有可以调用的程序。
FROM gcr.io/distroless/static-debian12:nonroot

COPY --from=build /out/server /server

# 时区：代码里大量使用 time.Local（日程按本地时间排布）。
# distroless 不带 /usr/share/zoneinfo,所以 main.go 里 import 了 time/tzdata
# 把时区库编进二进制 —— 这个 TZ 才有东西可查。
ENV TZ=Asia/Shanghai

# nonroot 用户，UID 65532。不是 root 意味着即使容器被攻破，也拿不到
# 容器里任何位置的写权限（根文件系统在 k8s 里还是只读挂载）。
USER nonroot:nonroot

EXPOSE 8080

# 数据在外部 MySQL 上：容器读环境变量 MYSQL_DSN 去连（见 cmd/server/main.go）。
# 镜像里没有任何持久化位置 —— 没配 DSN 就会退回 SQLite,而这里的文件系统
# 是只读的，起不来。这是想要的结果：线上不该出现「数据写进了容器」这种事。
ENTRYPOINT ["/server"]
CMD ["-addr", ":8080"]
