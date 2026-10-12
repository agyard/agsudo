# AG-sudo

AG-sudo 为 Linux 上的 Agent 工作流提供提权审批。Agent 保留普通用户权限；调用 `sudo` 时，由人或独立的审批 Agent 决定是否放行。

没有常驻服务，也没有内置模型：普通权限 CLI 通过按需运行的 setuid helper 管理请求、切换身份和执行命令，SQLite 保存状态与审计事件。命令行使用 Cobra，审批 TUI 使用 Bubble Tea、Bubbles 和 Lip Gloss。

## 安装

从 [GitHub Releases](https://github.com/thezzisu/agsudo/releases) 下载 Linux `amd64` 或 `arm64` 压缩包及 `checksums.txt`，校验并解压。压缩包内包含两个可执行文件和安装脚本，不需要 Go。

```sh
# 以 amd64 为例，在下载目录中执行
sha256sum --ignore-missing -c checksums.txt
tar -xzf agsudo_0.1.0_linux_amd64.tar.gz -C /path/to/empty-directory
cd /path/to/empty-directory
sudo ./scripts/install.sh "$(id -u)"
```

安装命令应从普通用户的 shell 运行，`id -u` 在提权前取到该用户的数字 UID。安装后：

```sh
agsudo doctor
agsudo review --tui
```

安装器将 CLI 放到 `/usr/local/bin/agsudo`（root，0755），helper 放到 `/usr/local/libexec/agsudo-helper`（root，4755）。初次安装会在 `/etc/agsudo/config.json` 配置该用户的提交与审批权限，状态目录为 `/var/lib/agsudo`（root，0700）。升级保留配置和数据库，不修改系统 sudoers。

需要 Linux、本地用户账户和允许 setuid 的执行环境；`nosuid` 挂载、`no_new_privs` 或宿主沙箱可能阻止提权。应用以静态 Go 二进制构建，不实现 PAM、LDAP/SSSD NSS 或完整 sudo 兼容。

## 接入 Agent

安装一次宿主集成，之后照常启动 Agent：

```sh
agsudo integration install codex
# 或
agsudo integration install claude
```

安装器合并用户级 hook 配置，保留其他设置，并备份首次修改前的原文件。重启宿主以加载配置。**Codex 接入用于 Full Access 会话，还需在 `/hooks` 中信任新增 hook。** 集成不自动修改宿主权限或信任记录。卸载使用 `agsudo integration uninstall codex` 或 `claude`。

其他 Agent 可以使用通用包装入口：

```sh
agsudo wrap -- your-agent
# 或打开一个可交互体验的 shell
agsudo wrap -- bash
```

在这些进程中，PATH 查找到的 `sudo` 会进入 AG-sudo。短暂等待保持安静，超过约两秒只显示一次：

```text
sudo: approval pending; see 'sudo --help'
```

批准后执行原命令，标准输入输出和退出码保留。`sudo --help` 提供进一步发现异步 CLI 的入口。宿主接入的工作方式和限制见 [integrations/README.md](integrations/README.md)。

## 人工审批

在另一个终端运行：

```sh
agsudo review --tui
```

TUI 自动显示普通待办和转人工请求，可查看命令、目标用户、目录、申请理由、文件快照和审批建议。`a` 批准、`d` 拒绝、`s` 转人工，填写可选理由后 Enter 确认；`/` 过滤，Tab 切换列表与详情，`t` 查看事件，`h` 切换历史，`q` 退出。

也可以使用命令行：

```sh
agsudo review
agsudo show req_ID
agsudo approve req_ID --reason '符合任务范围'
agsudo deny req_ID --reason '超出任务范围'
```

## 请求与异步执行

`exec` 等待审批并执行；`request` 只提交，返回请求 ID，不会自行启动命令。

```sh
agsudo exec --reason '安装构建依赖' -- apt-get install -y libssl-dev
agsudo request --json --context ./deployment-notes.txt -- systemctl restart example.service
agsudo status req_ID --json
agsudo exec --request req_ID
agsudo cancel req_ID --reason '任务已改变'
```

执行选项放在 COMMAND 之前，COMMAND 后的参数原样传递。请求内容固定，审批后不能替换参数或目标用户；授权只能执行一次，并从提交时起计算有效期。等待时 Ctrl-C 会取消申请；运行后的 Ctrl-C 传递给程序，`cancel` 只作用于尚未执行的请求。

支持 `-u USER`、`-n`、`-H`、无命令参数的 `-s/-i`、`--`、帮助和版本；其他 sudo 选项明确报错。交互式 shell 一次审批覆盖整个 shell 会话，其内部命令不再逐条审批。普通执行保留 cwd，`-i` 进入目标用户 home；执行环境经过清理，默认 umask 为 0077。

## 审批 Agent 与审计

审批者使用相同的 JSON CLI，不需要模型 SDK：

```sh
agsudo review --json
agsudo show req_ID --json
agsudo approve req_ID --expect-digest DIGEST --actor agent:reviewer --reason '范围明确' --json
agsudo defer req_ID --recommend approve --summary '安装开发依赖' --reason '需要用户确认软件源' --json
```

`defer` 转为 `needs_human`，从默认 Agent 待办队列移出，保留给 TUI 或 `review --state needs_human`。申请理由、文件和建议都是不可信材料；接入建议见 [docs/reviewer.md](docs/reviewer.md)。

```sh
agsudo watch --json --after 42
agsudo audit export --session ses_ID > trace.jsonl
```

审计包含提权请求、上下文、建议、最终决定、执行起止与退出状态；事件序号支持断线续读。普通命令、终端按键和命令输出不记录。状态与审计事件在同一事务内提交。记录可能包含参数和显式附加文件中的敏感数据；本机 root 可以修改数据库。格式和状态机见 [docs/protocol.md](docs/protocol.md)。

## 权限范围

`/etc/agsudo/config.json` 中的 `allowed_uids` 可提交并执行自己的请求，`reviewer_uids` 可查看和审批所有请求；root 可管理全部。审批者可以批准 root shell，因此应将审批权限视为可授权 root 操作的权限。同 UID 下主动自批不在隔离目标内。

PATH 接入不会拦截 `/usr/bin/sudo`、alias/function 或其他提权途径。可执行文件在提交时记录摘要、执行前重新检查，但这不冻结脚本依赖、配置文件或外部状态。完整边界见 [docs/architecture.md](docs/architecture.md)。

## 构建与测试

源码构建需要 Go 1.26.9+：

```sh
make build       # bin/agsudo 和 bin/agsudo-helper
make check       # go vet
make test        # 单元测试及 race 检测
make integration # CLI、真实终端和容器中的 setuid/安装测试
```

集成测试需要 Python 3、Bash 和 Docker；会按需拉取 Debian 测试镜像，不安装到宿主系统。ARM64 发布包由交叉编译验证，终端和提权测试在 Linux amd64 运行。

免 root 开发体验：

```sh
make demo
export AGSUDO_HELPER="$PWD/bin/agsudo-helper-dev"
export AGSUDO_DEV_STATE="$PWD/.agsudo-dev"
bin/agsudo exec -- id
# 另一个终端设置相同变量后运行 bin/agsudo review --tui
```

开发 helper 只允许当前 UID，拒绝 root 或 setuid 运行。生产二进制不包含开发路径覆盖。发布步骤见 [docs/releasing.md](docs/releasing.md)。

[Apache-2.0](LICENSE)。发布包附带第三方许可证。
