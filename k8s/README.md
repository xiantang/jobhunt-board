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
k3s 滚动更新（Recreate）
   ↓
Traefik ──BasicAuth──> Service ──> Pod ──> PVC(/data/app.db)
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

### 3. 应用密钥

```bash
k -n jobhunt create secret generic jobhunt-secrets \
  --from-literal=OPENAI_API_KEY='sk-...' \
  --from-literal=GOOGLE_CLIENT_ID='xxx.apps.googleusercontent.com' \
  --from-literal=GOOGLE_CLIENT_SECRET='...' \
  --from-literal=GOOGLE_REDIRECT_URL='https://jobhunt.vim0.com/oauth/google/callback'
```

Deployment 里是 `optional: true`，所以这个 Secret 不存在服务也能起来，只是
「✨ AI 录入」和 Google 日历同步两个入口不显示。

> ⚠️ 上线后必须去 [Google Cloud Console](https://console.cloud.google.com/apis/credentials)
> 把 `https://jobhunt.vim0.com/oauth/google/callback` 加进「Authorized redirect
> URIs」，**和这里的值一字不差**。本地那个 `http://localhost:8080/...` 可以并存，
> 两个都留着就能本地和线上都用。

### 4. 让 ArgoCD 发现这个应用

Application 清单在**博客仓库**里（`k8s/argocd/jobhunt-application.yaml`），
因为 root-application 只监管那个目录。推上去 ArgoCD 就会自己创建。

### 5. 先跑一次 CI

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

SQLite 文件在 PVC 上，实际落在**那台节点的本地磁盘**
（`/var/lib/rancher/k3s/storage/...`，local-path 供应器）。

⚠️ **这不是备份。** EC2 根卷是 `delete_on_termination = true`，机器一旦
terminate，这块数据跟着消失。PVC 上加了 `Prune=false` 和
`helm.sh/resource-policy: keep`，能挡住「ArgoCD 顺手删掉」，但挡不住机器没了。

导出一份：

```bash
POD=$(k -n jobhunt get pod -l app.kubernetes.io/name=jobhunt -o name | head -1)
k -n jobhunt exec "$POD" -- cat /data/app.db > app-$(date +%F).db
```

> 镜像是 distroless，容器里**没有 shell 也没有 sqlite3**，所以只能这样整个文件
> 拷出来，不能在容器里跑 `.backup`。服务在跑时直接 cat 理论上可能读到不一致的
> 快照（WAL 还没 checkpoint）；要严谨的话先把副本数缩到 0 再从节点上拷。

## 排障

| 症状 | 多半是 |
|---|---|
| Pod `ImagePullBackOff` | appVersion 还是占位值，或 CI 没跑成功 |
| Pod `CrashLoopBackOff`，日志报 `disk I/O error` | PVC 权限不对，检查 `fsGroup: 65532` |
| 页面能开但没有 401 | 中间件没挂上，见上面「必须验证的两件事」 |
| 证书一直 pending | `k -n jobhunt describe certificate`，多半是 Cloudflare token 或 DNS-01 记录的问题 |
| 日程时间整体偏 8 小时 | 镜像少了 `time/tzdata`（main.go 里那个匿名 import） |
| 推代码后本地 push 被拒 | CI 的 bot commit 回填了 appVersion，先 `git pull` |
