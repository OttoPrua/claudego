#!/bin/bash
# 模拟 claude CLI：行为由 $MOCK_DIR/plan 控制（每行一个行为，按调用次数消费）。
# 行为: ok | slow | hang | emit | coord | progress | limit | limit_at:<epoch> | err
set -u
PROMPT=$(cat)
mkdir -p "$MOCK_DIR"
n=$(cat "$MOCK_DIR/n" 2>/dev/null || echo 0)
n=$((n + 1))
echo "$n" > "$MOCK_DIR/n"

{
  echo "===== call $n args: $* thinking=${MAX_THINKING_TOKENS:-none}"
  echo "$PROMPT"
} >> "$MOCK_DIR/calls.log"

beh=$(sed -n "${n}p" "$MOCK_DIR/plan" 2>/dev/null)
[ -z "$beh" ] && beh=ok

case "$beh" in
  hang)
    # 模拟吊死的执行器：派生孙进程并记录 pid，供 cancel 的进程组击杀断言。
    sleep 300 &
    echo $! > "$MOCK_DIR/hang-child.pid"
    sleep 300
    ;;
  slow)
    sleep 1.2
    echo '{"type":"result","subtype":"success","is_error":false,"result":"slow ok","session_id":"sess-'"$n"'","num_turns":3,"total_cost_usd":0.01,"duration_ms":1200,"usage":{"input_tokens":800,"output_tokens":200,"cache_creation_input_tokens":0,"cache_read_input_tokens":0}}'
    ;;
  ok)
    echo '{"type":"result","subtype":"success","is_error":false,"result":"step ok","session_id":"sess-'"$n"'","num_turns":3,"total_cost_usd":0.01,"duration_ms":1200,"usage":{"input_tokens":800,"output_tokens":200,"cache_creation_input_tokens":0,"cache_read_input_tokens":0}}'
    ;;
  emit)
    echo '{"type":"result","subtype":"success","is_error":false,"result":"调研完成。\n```json\n{\"tasks\":[{\"title\":\"emitted-task\",\"type\":\"sequence\",\"priority\":7,\"review_after\":true,\"prompts\":[\"do step a\",\"do step b\"]}]}\n```","session_id":"sess-'"$n"'","num_turns":5,"total_cost_usd":0.02,"duration_ms":1500,"usage":{"input_tokens":800,"output_tokens":200,"cache_creation_input_tokens":0,"cache_read_input_tokens":0}}'
    ;;
  emit2)
    echo '{"type":"result","subtype":"success","is_error":false,"result":"拆分完成。\n```json\n{\"tasks\":[{\"title\":\"held-task\",\"type\":\"sequence\",\"priority\":4,\"model\":\"haiku\",\"fresh_steps\":true,\"prompts\":[\"do h1\"]}]}\n```","session_id":"sess-'"$n"'","num_turns":3,"total_cost_usd":0.01,"duration_ms":900,"usage":{"input_tokens":500,"output_tokens":150,"cache_creation_input_tokens":0,"cache_read_input_tokens":0}}'
    ;;
  coordchain)
    echo '{"type":"result","subtype":"success","is_error":false,"result":"链式分工。\n```json\n{\"tasks\":[{\"title\":\"chain-next\",\"type\":\"coordinate\",\"priority\":3,\"prompts\":[\"next round planning\"]}]}\n```","session_id":"sess-'"$n"'","num_turns":2,"total_cost_usd":0.01,"duration_ms":900,"usage":{"input_tokens":500,"output_tokens":100,"cache_creation_input_tokens":0,"cache_read_input_tokens":0}}'
    ;;
  coord)
    echo '{"type":"result","subtype":"success","is_error":false,"result":"分工方案：coord-task 用 haiku 跑机械整理。\n```json\n{\"tasks\":[{\"title\":\"coord-task\",\"type\":\"sequence\",\"priority\":6,\"model\":\"haiku\",\"prompts\":[\"do c1\"]}]}\n```","session_id":"sess-'"$n"'","num_turns":4,"total_cost_usd":0.02,"duration_ms":1400,"usage":{"input_tokens":800,"output_tokens":200,"cache_creation_input_tokens":0,"cache_read_input_tokens":0}}'
    ;;
  coord_badtype)
    echo '{"type":"result","subtype":"success","is_error":false,"result":"分工。\n```json\n{\"tasks\":[{\"title\":\"badtype-task\",\"type\":\"batch\",\"priority\":6,\"prompts\":[\"do bt1\"]}]}\n```","session_id":"sess-'"$n"'","num_turns":4,"total_cost_usd":0.02,"duration_ms":1400,"usage":{"input_tokens":800,"output_tokens":200,"cache_creation_input_tokens":0,"cache_read_input_tokens":0}}'
    ;;
  progress)
    echo '{"type":"result","subtype":"success","is_error":false,"result":"进度如下。\n```json\n{\"goal\":\"目标X\",\"done\":[\"a1\",\"a2\"],\"in_progress\":\"a3\",\"remaining\":[\"b1\"],\"blockers\":[],\"key_files\":[\"x.go\"],\"next_prompt\":\"continue b1\"}\n```","session_id":"sess-'"$n"'","num_turns":2,"total_cost_usd":0.005,"duration_ms":800,"usage":{"input_tokens":400,"output_tokens":100,"cache_creation_input_tokens":0,"cache_read_input_tokens":0}}'
    ;;
  limit)
    epoch=$(( $(date +%s) + 3600 ))
    echo "Claude AI usage limit reached|$epoch" >&2
    exit 1
    ;;
  limit_at:*)
    echo "Claude AI usage limit reached|${beh#limit_at:}" >&2
    exit 1
    ;;
  err)
    echo '{"type":"result","subtype":"error_during_execution","is_error":true,"result":"boom","session_id":"sess-'"$n"'","num_turns":1,"total_cost_usd":0.0,"duration_ms":500}'
    ;;
  review_concerns)
    echo '{"type":"result","subtype":"success","is_error":false,"result":"对抗审核报告：击破两处。\n```json\n{\"verdict\":\"concerns\",\"p0\":[\"x.mjs:10 恒真门,注入反例不报红,修法:改真实断言\"],\"p1\":[\"y.mjs:20 fail-open,修法:显式失败\"],\"p2\":[],\"summary\":\"两处承重缺陷\"}\n```","session_id":"sess-'"$n"'","num_turns":6,"total_cost_usd":0.03,"duration_ms":1600,"usage":{"input_tokens":900,"output_tokens":300,"cache_creation_input_tokens":0,"cache_read_input_tokens":0}}'
    ;;
  review_pass)
    echo '{"type":"result","subtype":"success","is_error":false,"result":"对抗审核报告：全部攻不破。\n```json\n{\"verdict\":\"pass\",\"p0\":[],\"p1\":[],\"p2\":[\"小建议\"],\"summary\":\"通过\"}\n```","session_id":"sess-'"$n"'","num_turns":4,"total_cost_usd":0.02,"duration_ms":1200,"usage":{"input_tokens":700,"output_tokens":200,"cache_creation_input_tokens":0,"cache_read_input_tokens":0}}'
    ;;
esac
