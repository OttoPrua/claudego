#!/bin/bash
# 模拟 ssh：末参为远端命令，stdin=prompt。记录调用；输出 codex 噪声 + marker + 假结果。
# 退出 3 模拟 Windows codex 非零退出——有结果即成功，不应导致任务失败。
set -u
PROMPT=$(cat)
mkdir -p "$MOCK_DIR"
{
  echo "===== ssh call args: $*"
  echo "PROMPT: $PROMPT"
} >> "$MOCK_DIR/ssh-calls.log"
echo "OpenAI Codex 执行日志噪声（marker 之前，应被解析器忽略）"
echo "===CLAUDEGO_REMOTE_RESULT==="
echo "remote step done"
exit 3
