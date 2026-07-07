#!/bin/bash
# 模拟 codex CLI：记录调用，把"最终消息"写到 -o 指定的文件。
set -u
out=""
prev=""
for a in "$@"; do
  [ "$prev" = "-o" ] && out="$a"
  prev="$a"
done
{
  echo "===== codex call args: $*"
} >> "$MOCK_DIR/codex-calls.log"
[ -n "$out" ] && printf 'codex done' > "$out"
echo "codex running"
