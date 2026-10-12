# Codex 与 Claude Code 接入

每个宿主只提供一种接入方式，由 AG-sudo 写入用户级 hook 配置：

```sh
agsudo integration install codex
agsudo integration install claude
agsudo integration uninstall codex
agsudo integration uninstall claude
```

请以普通用户运行已安装到稳定路径的 AG-sudo。安装器保存当前可执行文件的绝对路径；移动二进制后重新安装即可更新。配置缺失时创建，存在时合并，保留无关字段和其他 hook。重复安装不新增副本；卸载按 AG-sudo 标记识别 handler，保留同组其他 handler。

首次修改前的原文件保存在旁边的 `.agsudo.bak`，后续操作不覆盖该备份。无效 JSON、不支持的 hook 结构和符号链接配置会报错，原文件不改动。安装完成不代表宿主已经加载或信任 hook；重启宿主并检查其 hook 配置。

## Codex

安装到 `$CODEX_HOME/hooks.json`，未设置该变量时为 `~/.codex/hooks.json`。

`PreToolUse` 匹配宿主规范化后的 `Bash` 工具，包括 Codex 的 `exec_command`。Hook 在原命令前添加 PATH 和 `AGSUDO_SESSION` 环境设置，并保留其他工具参数。它不添加嵌套 shell，也不解析或替换命令中的 sudo 字符串，因此脚本内部通过 PATH 调用 sudo 也能接入。

Codex 的输入改写要求返回 `permissionDecision: allow`，所以本集成只接受 hook 输入中的 `permission_mode: bypassPermissions`（Full Access）。其他模式会拒绝该 Bash 调用，避免集成改变原有的审批预期。安装器不修改 sandbox、approval policy 或 hook trust；启用后须在 Codex `/hooks` 中审查并信任当前 hook。

Shell 自身仍可以修改 PATH、定义 sudo 函数或调用绝对路径；这些不在接入保证内。普通 Bash 命令经过环境设置，但只有实际调用 AG-sudo 才会写入提权审计。

## Claude Code

安装到 `$CLAUDE_CONFIG_DIR/settings.json`，未设置时为 `~/.claude/settings.json`。

`SessionStart` 与 `CwdChanged` hooks 将环境设置追加到宿主提供的 `CLAUDE_ENV_FILE`；Claude 的 Bash 工具在执行命令前读取它。重复执行环境设置不会在 PATH 最前端重复叠加同一 shim，工作目录变化后会再次应用。没有 `PermissionRequest` 自动批准 hook。

这是 Linux 本地 Bash 接入。远端任务必须在命令实际执行的机器上安装 AG-sudo；本地配置不会自动安装到远端。宿主的沙箱若禁止 setuid，安装 hook 也不会解除该限制。

## 会话与诊断

两种接入均导出 `AGSUDO_SESSION`，并使 `sudo` 和 `agsudo` 可通过 PATH 找到。可以查询当前会话的全部请求：

```sh
agsudo review --all --json --session "$AGSUDO_SESSION"
```

内部入口 `agsudo integration hook codex|claude` 读取宿主 JSON，不使用模型或联网。Hook 初始化错误会向宿主报告；Codex 的一般 hook 错误、被禁用或尚未信任的 hook 可能导致宿主继续原命令，所以 hook 不是对抗性安全边界。

文档核对版本：Codex CLI 0.162.1、Claude Code 2.1.268。仓库测试运行官方事件格式、安装后的实际 hook 命令及其生成的 shell 程序，不启动付费模型会话，也不宣称覆盖所有宿主版本。

官方依据：[Codex hooks](https://developers.openai.com/codex/hooks)；[Claude Code hooks](https://code.claude.com/docs/en/hooks#persist-environment-variables)。
