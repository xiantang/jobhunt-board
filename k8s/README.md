# 部署到 EC2 上的 k3s

线上地址 <https://jobhunt.vim0.com>，跑在和博客同一台 EC2 的 k3s 集群里。

## 全貌

```
push master
   ↓
GitHub Actions（.github/workflows/deploy.yml）
   ├─ go vet + go test        ← 不过就不发布
   ├─ 构建镜像 → ghcr.io/xiantang/jobhunt-board:sha-<commit>
   └─ 回填 charts/jobhunt/Chart.yaml 的 appVersion（bot commit）
   ↓
ArgoCD 轮询到这个 commit
   ↓
k3s 滚动更新（RollingUpdate）
   ↓
Traefik ──BasicAuth──> Service ──> Pod ──> MySQL StatefulSet（PVC）
   └─ 证书由 cert-manager 走 Cloudflare DNS-01 自动签发续期
```

**发布闸口是 `Chart.yaml` 的 `appVersion`，不是 push 本身。** 推代码只会构建出
一个镜像；只有 CI 把 appVersion 改成那个新 tag，ArgoCD 才看到变化。想临时停住
自动发布，注释掉 workflow 里最后那步即可，镜像照常构建。

## 一次性准备

下面这些**都不在 git 里**，换机器或重建集群时要照做一遍。

### 1. DNS

`jobhunt.vim0.com` 的 A 记录指向 k3s 那台机器的 EIP，由私有的 infra 仓库
（`blog/dns.tf` 里的 `cloudflare_dns_record.jobhunt`）管理：

```bash
cd infra/blog && terraform apply
```

### 2. 访问口令（BasicAuth）

看板本身没有任何登录功能，里面是真实的投递记录和面试信息。防护全靠 Traefik
在前面拦的这一层，**这个 Secret 不建，中间件会失效，看板就是裸奔状态**。

```bash
# htpasswd 来自 apache2-utils；没有的话用 openssl passwd -apr1 也行
htpasswd -nbB <用户名> '<密码>' > /tmp/auth
k create namespace jobhunt --dry-run=client -o yaml | k apply -f -
k -n jobhunt create secret generic jobhunt-basicauth \
  --from-file=users=/tmp/auth
shred -u /tmp/auth      # 别把明文留在磁盘上
```

> ⚠️ key 必须叫 `users`，Traefik 的 basicAuth 中间件只认这个名字。
> 叫别的不会报错，只会**静默地放行所有请求**。

### 3. MySQL（跑在集群里）

数据库由 chart 一起部署：一个单副本 StatefulSet + 一块 PVC + 一个只对集群内
开放的 ClusterIP Service（`values.yaml` 里的 `mysql.internal`）。不开 NodePort
也不进 Ingress —— 数据库不该有公网入口，要从本机连就 `port-forward`。

要手动建的是两个 Secret，**都不在 git 里**，而且刻意分开：

```bash
# ① 给 MySQL 自己：root 口令 + 应用账号
k -n jobhunt create secret generic jobhunt-mysql-auth \
  --from-literal=root-password="$(openssl rand -base64 24)" \
  --from-literal=username=jobhunt \
  --from-literal=password="$(openssl rand -base64 24)"

# ② 给应用：一整条 DSN。密码要和 ① 里的 password 一致
k -n jobhunt create secret generic jobhunt-mysql \
  --from-literal=dsn='jobhunt:<和①一样的密码>@tcp(jobhunt-mysql:3306)/jobhunt'
```

> 用 `openssl rand` 生成的话，先把 ① 建好再把密码读出来拼进 ②：
> `k -n jobhunt get secret jobhunt-mysql-auth -o jsonpath='{.data.password}' | base64 -d`

分成两个是因为泄露面不同：应用只该知道自己那条 DSN,拿不到 root。

DSN 是 `go-sql-driver/mysql` 的格式，写到库名就行——字符集、时区、超时那些参数
由代码统一钉死（`internal/platform/db/mysql.go`），不用也不该在这里配。
库和表也不用手动建：`MYSQL_DATABASE` 让镜像建库，服务启动时跑幂等的建表语句。

> ⚠️ `jobhunt-mysql` 这个 Secret 和下面应用密钥那个不一样，**不是 `optional`**：
> 它不存在时 Pod 会停在 `CreateContainerConfigError`。这是故意的——要是让它可选，
> 缺 DSN 的 Pod 会安静地退回 SQLite,把数据写进容器里，重启就没，而页面上一切正常。
> 宁可起不来。

启动顺序不用管：应用先起来时连不上库会直接退出，kubelet 重启它，
MySQL 就绪后自然连上（这个过程里会看到几次 `CrashLoopBackOff`,正常）。
第一次初始化数据目录要几十秒。

要手动连上去看：

```bash
k -n jobhunt port-forward svc/jobhunt-mysql 3306:3306
mysql -h 127.0.0.1 -u root -p
```

### 4. 应用密钥

```bash
k -n jobhunt create secret generic jobhunt-secrets \
  --from-literal=OPENAI_API_KEY='sk-...' \
  --from-literal=GOOGLE_CLIENT_ID='xxx.apps.googleusercontent.com' \
  --from-literal=GOOGLE_CLIENT_SECRET='...' \
  --from-literal=GOOGLE_REDIRECT_URL='https://jobhunt.vim0.com/oauth/google/callback'
```

本地 `.env` 里已经有这几个值的话，可以直接由它生成，省得手抄：

```bash
k -n jobhunt create secret generic jobhunt-secrets --from-env-file=.env
```

> ⚠️ 两件事要先看一眼再敲：
> - `.env` 里的 `GOOGLE_REDIRECT_URL` 指向的是 `http://localhost:8080/...`,
>   直接灌进去线上 OAuth 会回调到本机。灌完补一条覆盖它：
>   `k -n jobhunt create secret generic jobhunt-secrets --from-env-file=.env \
>   --from-literal=GOOGLE_REDIRECT_URL='https://jobhunt.vim0.com/oauth/google/callback' \
>   --dry-run=client -o yaml | k apply -f -`
> - `--from-env-file` 会把文件里【所有】键都变成环境变量。确认里面没有不该上
>   集群的东西（本地路径、调试开关）。

Deployment 里是 `optional: true`，所以这个 Secret 不存在服务也能起来，只是
「✨ AI 录入」和 Google 日历同步两个入口不显示。

> ⚠️ 上线后必须去 [Google Cloud Console](https://console.cloud.google.com/apis/credentials)
> 把 `https://jobhunt.vim0.com/oauth/google/callback` 加进「Authorized redirect
> URIs」，**和这里的值一字不差**。本地那个 `http://localhost:8080/...` 可以并存，
> 两个都留着就能本地和线上都用。

### 5. 让 ArgoCD 发现这个应用

Application 清单在**博客仓库**里（`k8s/argocd/jobhunt-application.yaml`），
因为 root-application 只监管那个目录。推上去 ArgoCD 就会自己创建。

### 6. 先跑一次 CI

`Chart.yaml` 里的 `appVersion` 初始是占位值 `sha-REPLACE_ME_FIRST_CI_RUN`。
直接同步的话 Pod 会卡在 `ImagePullBackOff`。所以顺序是：**先 push 一次触发 CI**，
等它把 appVersion 回填了，再让 ArgoCD 同步。

## 上线后必须验证的两件事

```bash
# 1. BasicAuth 真的生效了 —— 期望 401
curl -s -o /dev/null -w '%{http_code}\n' https://jobhunt.vim0.com/

# 2. 带上口令能进 —— 期望 200
curl -s -o /dev/null -w '%{http_code}\n' -u '<用户名>:<密码>' https://jobhunt.vim0.com/
```

第一条**尤其重要**。Traefik 在中间件引用写错（名字拼错、namespace 不对、
少了 `@kubernetescrd` 后缀）时的行为是**既不报错也不拦截**，Ingress 看起来
一切正常，实际上没有任何防护。只有这条 curl 能发现。

## 数据在哪，怎么备份

数据在集群里那个 MySQL 的 PVC 上（`data-jobhunt-mysql-0`）。它由
StatefulSet 的 `volumeClaimTemplates` 生成，**删掉 StatefulSet 也不会跟着删**,
要手动 `delete pvc` 才会没。

⚠️ 但 k3s 默认的 local-path 意味着这块盘就是**那台 EC2 节点的本地磁盘**
（`/var/lib/rancher/k3s/storage/...`）。根卷是 `delete_on_termination = true`,
机器一 terminate，数据跟着消失。**PVC 不是备份。**

导一份出来：

```bash
POD=jobhunt-mysql-0
k -n jobhunt exec "$POD" -- sh -c \
  'mysqldump -u root -p"$MYSQL_ROOT_PASSWORD" --single-transaction \
   --default-character-set=utf8mb4 jobhunt' > jobhunt-$(date +%F).sql
```

`--single-transaction` 走一致性快照，不用停服务；口令从容器自己的环境变量取，
不会出现在你的 shell history 里。

灌回去：

```bash
k -n jobhunt exec -i jobhunt-mysql-0 -- sh -c \
  'mysql -u root -p"$MYSQL_ROOT_PASSWORD" jobhunt' < jobhunt-2026-08-22.sql
```

> 本地开发仍然是 SQLite（不配 `MYSQL_DSN` 就走 `data/app.db`），
> 两边跑的是同一套 SQL——`TEST_MYSQL_DSN=... go test ./...` 会把整个测试套件
> 在 MySQL 上再跑一遍，见根目录 README。

## 排障

| 症状 | 多半是 |
|---|---|
| Pod `ImagePullBackOff` | appVersion 还是占位值，或 CI 没跑成功 |
| Pod 停在 `CreateContainerConfigError` | `jobhunt-mysql` 这个 Secret 没建，或者 key 不叫 `dsn` |
| 应用 `CrashLoopBackOff`,日志报 `连接数据库失败` | MySQL 还没就绪（等一会自己会好）；一直不好就查 DSN 里的口令和 `jobhunt-mysql-auth` 对不对得上 |
| `jobhunt-mysql-0` 起不来，日志报 `Can't create/write to file` | 数据目录属主不对，检查 `runAsUser/fsGroup: 999` |
| 页面能开但没有 401 | 中间件没挂上，见上面「必须验证的两件事」 |
| 证书一直 pending | `k -n jobhunt describe certificate`，多半是 Cloudflare token 或 DNS-01 记录的问题 |
| 日程时间整体偏 8 小时 | 镜像少了 `time/tzdata`（main.go 里那个匿名 import） |
| 推代码后本地 push 被拒 | CI 的 bot commit 回填了 appVersion，先 `git pull` |
