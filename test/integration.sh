#!/bin/bash
# 集成测试：用 mock claude 验证 调度顺序 / 限额暂停+冷却 / 自动续跑 / 装配产出入队 / 完成后自动审核
# / 模型路由 / 进度自动回收 / 分工协调（实时快照注入 + 带模型入队）。
set -euo pipefail
cd "$(dirname "$0")/.."

go build -o bin/claudego .
BIN="$PWD/bin/claudego"

TMP=$(mktemp -d)
trap 'rm -rf "$TMP"' EXIT
export CLAUDEGO_ROOT="$TMP/root"
export MOCK_DIR="$TMP/mock"
mkdir -p "$MOCK_DIR"
PROJ="$TMP/proj" && mkdir -p "$PROJ"

pass=0; fail=0
assert() { # assert <描述> <python 表达式，tasks 为任务列表>
  local desc="$1" expr="$2"
  if "$BIN" list -json | python3 -c "
import json,sys,time
tasks=json.load(sys.stdin) or []
byid={t['id']:t for t in tasks}
def one(**kw):
    m=[t for t in tasks if all(t.get(k)==v for k,v in kw.items())]
    assert len(m)==1, f'expect 1 match for {kw}, got {len(m)}'
    return m[0]
now=time.time()
assert $expr
"; then
    echo "  ✔ $desc"; pass=$((pass+1))
  else
    echo "  ✖ $desc"; fail=$((fail+1))
  fi
}

echo "== init =="
"$BIN" init >/dev/null
python3 - "$CLAUDEGO_ROOT/config.json" "$PWD/test/mock-claude.sh" <<'EOF'
import json,sys
p,mock=sys.argv[1],sys.argv[2]
cfg=json.load(open(p))
cfg["claude_bin"]=mock
cfg["limit_fallback_min"]=1
cfg["retry_backoff_min"]=1
json.dump(cfg,open(p,"w"),indent=2,ensure_ascii=False)
EOF
chmod +x test/mock-claude.sh test/mock-codex.sh

echo "== 场景1+2: 优先级顺序 + 单次 run 排空 + 第2步撞限额冷却 =="
"$BIN" add -dir "$PROJ" -title low-prio -priority 1 -file /dev/stdin <<'EOF' >/dev/null
step one of A
---
step two of A
EOF
"$BIN" review -dir "$PROJ" -priority 5 -title high-review "focus" >/dev/null
printf 'ok\nok\nlimit\n' > "$MOCK_DIR/plan"; echo 0 > "$MOCK_DIR/n"
"$BIN" run -quiet
assert "高优先级 review 完成" "one(title='high-review')['status']=='done'"
L1=$(grep -n "审核关注点：focus" "$MOCK_DIR/calls.log" | head -1 | cut -d: -f1)
L2=$(grep -n "step one of A" "$MOCK_DIR/calls.log" | head -1 | cut -d: -f1)
[ -n "$L1" ] && [ -n "$L2" ] && [ "$L1" -lt "$L2" ] && echo "  ✔ 高优先级先于低优先级派发" && pass=$((pass+1)) || { echo "  ✖ 派发顺序错误"; fail=$((fail+1)); }
assert "低优先级任务在同一次 run 内接续执行并撞限额暂停" "one(title='low-prio')['status']=='limit_paused'"
assert "步骤停在 1/2 且标记中断" "one(title='low-prio')['step']==1 and one(title='low-prio')['mid_step']==True"
assert "resume_at 在未来约1小时内" "0 < one(title='low-prio')['resume_at_epoch']-now < 3700"
test -f "$CLAUDEGO_ROOT/cooldown.json" && echo "  ✔ 冷却文件已写入" && pass=$((pass+1)) || { echo "  ✖ 缺少冷却文件"; fail=$((fail+1)); }

echo "== 场景3: 冷却期内不派发 =="
calls_before=$(cat "$MOCK_DIR/n")
"$BIN" run -quiet
calls_after=$(cat "$MOCK_DIR/n")
if [ "$calls_before" = "$calls_after" ]; then echo "  ✔ 冷却期内未调用 claude"; pass=$((pass+1)); else echo "  ✖ 冷却期内仍调用了 claude"; fail=$((fail+1)); fi

echo "== 场景4: 到点自动续跑（同会话 --resume + 续跑提示）=="
python3 - "$CLAUDEGO_ROOT" <<'EOF'
import json,glob,sys,time,os
root=sys.argv[1]
past=int(time.time())-5
os.remove(os.path.join(root,"cooldown.json"))
for f in glob.glob(os.path.join(root,"tasks","*.json")):
    t=json.load(open(f))
    if t["status"]=="limit_paused":
        t["resume_at_epoch"]=past
        json.dump(t,open(f,"w"),ensure_ascii=False)
EOF
printf 'ok\nok\nlimit\nok\n' > "$MOCK_DIR/plan"
"$BIN" run -quiet
assert "续跑后任务完成" "one(title='low-prio')['status']=='done' and one(title='low-prio')['step']==2"
grep -q -- "--resume sess-2" "$MOCK_DIR/calls.log" && echo "  ✔ 使用 --resume 续接原会话" && pass=$((pass+1)) || { echo "  ✖ 未用 --resume 续接"; fail=$((fail+1)); }
grep -q "上一条指令因为用量限额被中断" "$MOCK_DIR/calls.log" && echo "  ✔ 发送了续跑提示而非重发原 prompt" && pass=$((pass+1)) || { echo "  ✖ 未发送续跑提示"; fail=$((fail+1)); }

echo "== 场景5: 装配产出入队 → 同一次 run 内接力执行 → review_after 链 =="
printf 'emit\nok\nok\nok\n' > "$MOCK_DIR/plan"; echo 0 > "$MOCK_DIR/n"
"$BIN" assemble -dir "$PROJ" -title assembly-1 "build a thing" >/dev/null
"$BIN" run -quiet   # 排空：装配 → 产出任务 2 步 → 自动审核，一气呵成
assert "装配任务完成" "one(title='assembly-1')['status']=='done'"
assert "产出任务被接力执行完成且带 review_after" "one(title='emitted-task')['status']=='done' and one(title='emitted-task')['review_after']==True and len(one(title='emitted-task')['prompts'])==2"
assert "review_after 审核任务也在同轮完成" "one(title='审核: emitted-task')['status']=='done' and one(title='审核: emitted-task')['type']=='design-review'"

echo "== 场景6: 失败重试与退避 =="
printf 'err\n' > "$MOCK_DIR/plan"; echo 0 > "$MOCK_DIR/n"
"$BIN" add -dir "$PROJ" -title flaky -priority 9 "do flaky" >/dev/null
"$BIN" run -quiet
assert "失败后回到排队并带退避时间" "one(title='flaky')['status']=='queued' and one(title='flaky')['attempts']==1 and one(title='flaky')['not_before_epoch']>now"

echo "== 场景7: 任务级模型路由（--model 透传）=="
printf 'ok\n' > "$MOCK_DIR/plan"; echo 0 > "$MOCK_DIR/n"
"$BIN" add -dir "$PROJ" -title with-model -priority 10 -model sonnet "modeled step" >/dev/null
"$BIN" run -quiet
assert "指定模型的任务完成且记录模型" "one(title='with-model')['status']=='done' and one(title='with-model')['model']=='sonnet'"
grep -q -- "--model sonnet" "$MOCK_DIR/calls.log" && echo "  ✔ 向 claude 传递了 --model sonnet" && pass=$((pass+1)) || { echo "  ✖ 未传递 --model"; fail=$((fail+1)); }

echo "== 场景8: brief -auto 自动回收会话进度（haiku + --resume + 落盘）=="
printf 'progress\n' > "$MOCK_DIR/plan"; echo 0 > "$MOCK_DIR/n"
"$BIN" brief -session sess-42 -dir "$PROJ" -title probe -auto >/dev/null
"$BIN" run -quiet
assert "回收任务完成且默认 haiku" "one(title='进度: probe')['status']=='done' and one(title='进度: probe')['type']=='progress-pull' and one(title='进度: probe')['model']=='haiku'"
grep -q -- "--resume sess-42" "$MOCK_DIR/calls.log" && echo "  ✔ 回收任务续接了目标会话" && pass=$((pass+1)) || { echo "  ✖ 未 --resume 目标会话"; fail=$((fail+1)); }
grep -q -- "--model haiku" "$MOCK_DIR/calls.log" && echo "  ✔ 回收任务用 haiku 模型" && pass=$((pass+1)) || { echo "  ✖ 未用 haiku"; fail=$((fail+1)); }
test -f "$CLAUDEGO_ROOT/progress/s-sess-42.json" && echo "  ✔ 进度报告已落盘" && pass=$((pass+1)) || { echo "  ✖ 进度报告未落盘"; fail=$((fail+1)); }
plist=$("$BIN" progress)
echo "$plist" | grep -q "s-sess-42" && echo "  ✔ progress 列表可见" && pass=$((pass+1)) || { echo "  ✖ progress 列表不可见"; fail=$((fail+1)); }
# 8b: 会话不按 JSON 格式回复时，原文兜底落盘（不丢已花额度换来的汇报）
printf 'ok\n' > "$MOCK_DIR/plan"; echo 0 > "$MOCK_DIR/n"
"$BIN" brief -session sess-77 -dir "$PROJ" -title rawprobe -auto >/dev/null
"$BIN" run -quiet
assert "非 JSON 汇报的回收任务不失败" "one(title='进度: rawprobe')['status']=='done'"
"$BIN" brief -session sess-88 -dir "$PROJ" -title modelprobe -model sonnet -auto >/dev/null
assert "brief -model 覆盖回收模型" "one(title='进度: modelprobe')['model']=='sonnet'"
"$BIN" cancel "$("$BIN" list -json | python3 -c "import json,sys;print([t['id'] for t in json.load(sys.stdin) if t['title']=='进度: modelprobe'][0])")" >/dev/null
grep -q '"raw"' "$CLAUDEGO_ROOT/progress/s-sess-77.json" && echo "  ✔ 散文汇报以原文兜底落盘" && pass=$((pass+1)) || { echo "  ✖ 原文兜底未生效"; fail=$((fail+1)); }

echo "== 场景9: plan 分工协调（实时快照注入 + 产出任务带模型自动入队）=="
fid=$("$BIN" list -json | python3 -c "import json,sys;print([t['id'] for t in json.load(sys.stdin) if t['title']=='flaky'][0])")
"$BIN" cancel "$fid" >/dev/null
printf 'coord\nok\n' > "$MOCK_DIR/plan"; echo 0 > "$MOCK_DIR/n"
"$BIN" plan -dir "$PROJ" -priority 10 -title coord-plan "把剩余工作分工推进" >/dev/null
"$BIN" run -quiet   # 排空：协调 → 产出任务接力执行
assert "协调任务完成（默认最强模型 opus）" "one(title='coord-plan')['status']=='done' and one(title='coord-plan')['type']=='coordinate' and one(title='coord-plan')['model']=='opus'"
grep -q "目标X" "$MOCK_DIR/calls.log" && echo "  ✔ 协调 prompt 注入了进度报告" && pass=$((pass+1)) || { echo "  ✖ 未注入进度报告"; fail=$((fail+1)); }
grep -q "{{QUEUE}}" "$MOCK_DIR/calls.log" && { echo "  ✖ 队列占位符未被替换"; fail=$((fail+1)); } || { echo "  ✔ 队列快照已实时注入"; pass=$((pass+1)); }
assert "分工产出任务接力完成且带模型" "one(title='coord-task')['status']=='done' and one(title='coord-task')['model']=='haiku'"
grep -q -- "--model haiku" "$MOCK_DIR/calls.log" && echo "  ✔ 分工任务按建议模型执行" && pass=$((pass+1)) || { echo "  ✖ 分工任务未按模型执行"; fail=$((fail+1)); }

echo "== 场景10: progress -in 手动导入 与 cmd 手动接管 =="
echo '{"goal":"g2","done":["x"],"remaining":["y"]}' | "$BIN" progress -in -key manual-1 >/dev/null
plist=$("$BIN" progress)
echo "$plist" | grep -q "manual-1" && echo "  ✔ 手动导入进度可见" && pass=$((pass+1)) || { echo "  ✖ 手动导入失败"; fail=$((fail+1)); }
cid=$("$BIN" list -json | python3 -c "import json,sys;print([t['id'] for t in json.load(sys.stdin) if t['title']=='coord-task'][0])")
cout=$("$BIN" cmd "$cid")
echo "$cout" | grep -q -- "--model haiku" && echo "  ✔ cmd 输出手动接管命令（含模型）" && pass=$((pass+1)) || { echo "  ✖ cmd 输出缺少模型"; fail=$((fail+1)); }

echo "== 场景11: sessions 发现桌面端/CLI 会话（共用 ~/.claude/projects）=="
CCHOME="$TMP/cchome"
ENC=$(printf '%s' "$PROJ" | sed 's/[^a-zA-Z0-9]/-/g')
mkdir -p "$CCHOME/projects/$ENC"
cat > "$CCHOME/projects/$ENC/aaaa-bbbb-cccc.jsonl" <<'EOF'
{"type":"queue-operation","operation":"enqueue","sessionId":"aaaa-bbbb-cccc"}
{"parentUuid":null,"type":"user","message":{"role":"user","content":"给上传模块加断点续传"},"uuid":"u1"}
EOF
out=$(CLAUDE_CONFIG_DIR="$CCHOME" "$BIN" sessions -dir "$PROJ")
echo "$out" | grep -q "aaaa-bbbb-cccc" && echo "  ✔ 列出会话 ID" && pass=$((pass+1)) || { echo "  ✖ 未列出会话"; fail=$((fail+1)); }
echo "$out" | grep -q "断点续传" && echo "  ✔ 解析首条用户消息作标题" && pass=$((pass+1)) || { echo "  ✖ 标题解析失败"; fail=$((fail+1)); }
echo "$out" | grep -q "brief -session" && echo "  ✔ 给出接管命令提示" && pass=$((pass+1)) || { echo "  ✖ 缺少命令提示"; fail=$((fail+1)); }

echo "== 场景12: review/assemble 挂到既有角色会话（-session）=="
"$BIN" review -dir "$PROJ" -session role-review-1 -title attach-review "增量审查" >/dev/null
"$BIN" assemble -dir "$PROJ" -session role-asm-1 -title attach-asm "下一轮目标" >/dev/null
assert "审核任务挂上既有审核会话" "one(title='attach-review')['session_id']=='role-review-1'"
assert "装配任务挂上既有装配会话" "one(title='attach-asm')['session_id']=='role-asm-1'"

echo "== 场景13: 队列预算红线（本地加权账本）=="
python3 - "$CLAUDEGO_ROOT/config.json" <<'EOF'
import json,sys
p=sys.argv[1]; c=json.load(open(p)); c["queue_budget_tokens"]=1
json.dump(c,open(p,"w"),indent=2,ensure_ascii=False)
EOF
printf 'ok\n' > "$MOCK_DIR/plan"; echo 0 > "$MOCK_DIR/n"
"$BIN" run -quiet
[ "$(cat "$MOCK_DIR/n")" = "0" ] && echo "  ✔ 红线阻止派发（未调用 claude）" && pass=$((pass+1)) || { echo "  ✖ 红线未生效"; fail=$((fail+1)); }
lout=$("$BIN" list); echo "$lout" | grep -q "额度红线" && echo "  ✔ 看板显示红线横幅" && pass=$((pass+1)) || { echo "  ✖ 看板无红线提示"; fail=$((fail+1)); }
qout=$("$BIN" quota); echo "$qout" | grep -q "红线生效" && echo "  ✔ quota 显示触线状态" && pass=$((pass+1)) || { echo "  ✖ quota 未显示触线"; fail=$((fail+1)); }
"$BIN" run -quiet -force
[ "$(cat "$MOCK_DIR/n")" = "2" ] && echo "  ✔ -force 可越线并排空（attach 两任务都跑了）" && pass=$((pass+1)) || { echo "  ✖ -force 越线执行数不对（$(cat "$MOCK_DIR/n")）"; fail=$((fail+1)); }

echo "== 场景14: 外部用量源红线（CodexBar usage-history 格式）=="
FEED="$TMP/usage-history.jsonl"
NOWISO=$(python3 -c "import datetime;print(datetime.datetime.now(datetime.timezone.utc).strftime('%Y-%m-%dT%H:%M:%SZ'))")
printf '{"provider":"claude","sampledAt":"%s","resetsAt":"2026-12-31T00:00:00Z","usedPercent":93,"windowKind":"primary","windowMinutes":300}\n' "$NOWISO" > "$FEED"
python3 - "$CLAUDEGO_ROOT/config.json" "$FEED" <<'EOF'
import json,sys
p,feed=sys.argv[1],sys.argv[2]; c=json.load(open(p))
c["queue_budget_tokens"]=0; c["redline_percent"]=90; c["usage_feed"]=feed
json.dump(c,open(p,"w"),indent=2,ensure_ascii=False)
EOF
"$BIN" add -dir "$PROJ" -title feed-task -priority 2 "ft" >/dev/null
printf 'ok\n' > "$MOCK_DIR/plan"; echo 0 > "$MOCK_DIR/n"
"$BIN" run -quiet
[ "$(cat "$MOCK_DIR/n")" = "0" ] && echo "  ✔ 全局用量 93%≥90% 阻止派发" && pass=$((pass+1)) || { echo "  ✖ 全局红线未生效"; fail=$((fail+1)); }
printf '{"provider":"claude","sampledAt":"%s","resetsAt":"2026-12-31T00:00:00Z","usedPercent":50,"windowKind":"primary","windowMinutes":300}\n' "$NOWISO" > "$FEED"
"$BIN" run -quiet
[ "$(cat "$MOCK_DIR/n")" = "1" ] && echo "  ✔ 用量降到 50% 后恢复派发" && pass=$((pass+1)) || { echo "  ✖ 低于红线未放行"; fail=$((fail+1)); }

echo "== 场景15: 分时段红线（每日窗口内覆盖阈值，窗口外回落全局）=="
"$BIN" add -dir "$PROJ" -title windowed -priority 3 "wnd step" >/dev/null
IN_FROM=$(python3 -c "import datetime;print((datetime.datetime.now()-datetime.timedelta(minutes=10)).strftime('%H:%M'))")
IN_TO=$(python3 -c "import datetime;print((datetime.datetime.now()+datetime.timedelta(minutes=50)).strftime('%H:%M'))")
python3 - "$CLAUDEGO_ROOT/config.json" "$IN_FROM" "$IN_TO" <<'EOF'
import json,sys
p,f,t=sys.argv[1:4]; c=json.load(open(p))
c["queue_budget_tokens"]=0; c["redline_percent"]=0; c["usage_feed"]=""
c["redline_windows"]=[{"from":f,"to":t,"queue_budget_tokens":1}]
json.dump(c,open(p,"w"),indent=2,ensure_ascii=False)
EOF
printf 'ok\n' > "$MOCK_DIR/plan"; echo 0 > "$MOCK_DIR/n"
"$BIN" run -quiet
[ "$(cat "$MOCK_DIR/n")" = "0" ] && echo "  ✔ 时段内红线阻止派发" && pass=$((pass+1)) || { echo "  ✖ 时段内未阻止"; fail=$((fail+1)); }
qout=$("$BIN" quota); echo "$qout" | grep -q "当前时段生效" && echo "  ✔ quota 标记当前时段" && pass=$((pass+1)) || { echo "  ✖ quota 未标记时段"; fail=$((fail+1)); }
OUT_FROM=$(python3 -c "import datetime;print((datetime.datetime.now()+datetime.timedelta(hours=2)).strftime('%H:%M'))")
OUT_TO=$(python3 -c "import datetime;print((datetime.datetime.now()+datetime.timedelta(hours=3)).strftime('%H:%M'))")
python3 - "$CLAUDEGO_ROOT/config.json" "$OUT_FROM" "$OUT_TO" <<'EOF'
import json,sys
p,f,t=sys.argv[1:4]; c=json.load(open(p))
c["redline_windows"]=[{"from":f,"to":t,"queue_budget_tokens":1}]
json.dump(c,open(p,"w"),indent=2,ensure_ascii=False)
EOF
"$BIN" run -quiet
assert "时段外回落全局（无红线）正常执行" "one(title='windowed')['status']=='done'"

echo "== 场景16: 单次 run 排空队列 + 跨目录并行 =="
PROJ2="$TMP/proj2" && mkdir -p "$PROJ2"
python3 - "$CLAUDEGO_ROOT/config.json" <<'EOF'
import json,sys
p=sys.argv[1]; c=json.load(open(p))
c["max_parallel"]=2; c["redline_windows"]=[]
json.dump(c,open(p,"w"),indent=2,ensure_ascii=False)
EOF
printf 'slow\nslow\n' > "$MOCK_DIR/plan"; echo 0 > "$MOCK_DIR/n"
"$BIN" add -dir "$PROJ"  -title par-a -priority 2 "pa" >/dev/null
"$BIN" add -dir "$PROJ2" -title par-b -priority 2 "pb" >/dev/null
T0=$(python3 -c "import time;print(time.time())")
"$BIN" run -quiet
EL=$(python3 -c "import time,sys;print(time.time()-$T0)")
assert "一次 run 排空两个任务" "one(title='par-a')['status']=='done' and one(title='par-b')['status']=='done'"
python3 -c "import sys;sys.exit(0 if $EL < 2.2 else 1)" && echo "  ✔ 跨目录并行（${EL%.*}s < 2.2s）" && pass=$((pass+1)) || { echo "  ✖ 未并行（耗时 ${EL}s）"; fail=$((fail+1)); }

echo "== 场景17: 同目录强制串行 =="
printf 'slow\nslow\n' > "$MOCK_DIR/plan"; echo 0 > "$MOCK_DIR/n"
"$BIN" add -dir "$PROJ" -title ser-a -priority 2 "sa" >/dev/null
"$BIN" add -dir "$PROJ" -title ser-b -priority 2 "sb" >/dev/null
T0=$(python3 -c "import time;print(time.time())")
"$BIN" run -quiet
EL=$(python3 -c "import time,sys;print(time.time()-$T0)")
assert "同目录两任务都完成（仍在一次 run 内）" "one(title='ser-a')['status']=='done' and one(title='ser-b')['status']=='done'"
python3 -c "import sys;sys.exit(0 if $EL > 2.2 else 1)" && echo "  ✔ 同目录串行执行（${EL%.*}s > 2.2s）" && pass=$((pass+1)) || { echo "  ✖ 同目录被并行了（耗时 ${EL}s）"; fail=$((fail+1)); }

echo "== 场景18: claude 冷却期 codex 备用执行器接管单步任务 =="
python3 - "$CLAUDEGO_ROOT/config.json" "$PWD/test/mock-codex.sh" <<'EOF'
import json,sys,time
p,mock=sys.argv[1],sys.argv[2]
c=json.load(open(p))
c["codex_bin"]=mock; c["codex_fallback"]=True
json.dump(c,open(p,"w"),indent=2,ensure_ascii=False)
# 模拟 claude 撞限额：写入未来 1 小时的全局冷却
json.dump({"until_epoch":int(time.time())+3600,"reason":"mock limit","set_at":"t"},
          open(p.replace("config.json","cooldown.json"),"w"))
EOF
"$BIN" add -dir "$PROJ"  -title cx-single -priority 2 "single step work" >/dev/null
"$BIN" add -dir "$PROJ2" -title cx-multi -priority 9 -file /dev/stdin <<'EOF' >/dev/null
multi step one
---
multi step two
EOF
echo 0 > "$MOCK_DIR/n"
"$BIN" run -quiet
assert "单步任务被 codex 接管完成" "one(title='cx-single')['status']=='done' and one(title='cx-single')['runner']=='codex'"
assert "多步任务不切 codex，等待 claude 重置" "one(title='cx-multi')['status']=='queued'"
[ "$(cat "$MOCK_DIR/n")" = "0" ] && echo "  ✔ 冷却期未调用 claude" && pass=$((pass+1)) || { echo "  ✖ 冷却期调用了 claude"; fail=$((fail+1)); }
grep -q "sandbox read-only" "$MOCK_DIR/codex-calls.log" 2>/dev/null || grep -q -- "--sandbox workspace-write" "$MOCK_DIR/codex-calls.log" && echo "  ✔ codex 以受限沙箱执行" && pass=$((pass+1)) || { echo "  ✖ codex 沙箱参数缺失"; fail=$((fail+1)); }
grep -q "writable_roots.*\.git" "$MOCK_DIR/codex-calls.log" && echo "  ✔ codex 放行 .git 供收工 commit" && pass=$((pass+1)) || { echo "  ✖ 缺 .git writable_roots"; fail=$((fail+1)); }
rm -f "$CLAUDEGO_ROOT/cooldown.json"
printf 'ok\nok\n' > "$MOCK_DIR/plan"; echo 0 > "$MOCK_DIR/n"
"$BIN" run -quiet
assert "冷却解除后多步任务由 claude 执行完成" "one(title='cx-multi')['status']=='done' and one(title='cx-multi').get('runner') is None"

echo "== 场景19: fresh_steps 每步全新会话 + emit -hold 人工把关 =="
RB=$(grep -c -- "--resume" "$MOCK_DIR/calls.log")
"$BIN" add -dir "$PROJ" -title ff -priority 2 -fresh -file /dev/stdin <<'EOF' >/dev/null
fresh step one
---
fresh step two
EOF
printf 'ok\nok\n' > "$MOCK_DIR/plan"; echo 0 > "$MOCK_DIR/n"
"$BIN" run -quiet
assert "fresh_steps 任务完成且不留会话" "one(title='ff')['status']=='done' and one(title='ff').get('session_id') is None"
RA=$(grep -c -- "--resume" "$MOCK_DIR/calls.log")
[ "$RA" = "$RB" ] && echo "  ✔ 步骤间未使用 --resume（每步全新会话）" && pass=$((pass+1)) || { echo "  ✖ fresh_steps 仍复用了会话"; fail=$((fail+1)); }
"$BIN" assemble -dir "$PROJ2" -title asm-hold -hold "拆一个目标" >/dev/null
printf 'emit2\n' > "$MOCK_DIR/plan"; echo 0 > "$MOCK_DIR/n"
"$BIN" run -quiet
assert "-hold 产出任务挂起待审" "one(title='held-task')['status']=='held' and one(title='held-task')['fresh_steps']==True"
hid=$("$BIN" list -json | python3 -c "import json,sys;print([t['id'] for t in json.load(sys.stdin) if t['title']=='held-task'][0])")
"$BIN" release "$hid" >/dev/null
assert "release 放行进入排队" "one(title='held-task')['status']=='queued'"

echo "== 场景19: runner=codex 常态钉定（claude 空闲也走 codex）+ 思考等级透传 =="
python3 - "$CLAUDEGO_ROOT/config.json" <<'EOF'
import json,sys
p=sys.argv[1]; c=json.load(open(p))
c["thinking_tokens"]=12345; c["codex_reasoning"]="high"
json.dump(c,open(p,"w"),indent=2,ensure_ascii=False)
EOF
printf 'ok\n' > "$MOCK_DIR/plan"; echo 0 > "$MOCK_DIR/n"
"$BIN" add -dir "$PROJ"  -runner codex -title pin-codex -priority 2 "filler audit" >/dev/null
"$BIN" add -dir "$PROJ2" -title think-claude -priority 2 "think work" >/dev/null
"$BIN" run -quiet
assert "钉定任务在 claude 空闲时仍走 codex" "one(title='pin-codex')['status']=='done' and one(title='pin-codex')['runner']=='codex'"
assert "普通任务同轮由 claude 执行" "one(title='think-claude')['status']=='done' and one(title='think-claude').get('runner') is None"
grep -q "model_reasoning_effort=high" "$MOCK_DIR/codex-calls.log" && echo "  ✔ codex 推理等级已拉高" && pass=$((pass+1)) || { echo "  ✖ codex 推理等级未透传"; fail=$((fail+1)); }
grep -q "thinking=12345" "$MOCK_DIR/calls.log" && echo "  ✔ claude 思考预算已透传" && pass=$((pass+1)) || { echo "  ✖ MAX_THINKING_TOKENS 未透传"; fail=$((fail+1)); }
"$BIN" add -dir "$PROJ" -runner codex -title bad-pin -file /dev/stdin <<'EOF' >/dev/null 2>&1 && { echo "  ✖ 多步非 fresh 任务不该允许钉 codex"; fail=$((fail+1)); } || { echo "  ✔ 多步非 fresh 钉 codex 被拒绝"; pass=$((pass+1)); }
s1
---
s2
EOF

echo "== 场景20: 红线前置缓冲（踩线起跑防护，codex 钉定不受影响）=="
LEAD_FROM=$(python3 -c "import datetime;print((datetime.datetime.now()+datetime.timedelta(minutes=10)).strftime('%H:%M'))")
LEAD_TO=$(python3 -c "import datetime;print((datetime.datetime.now()+datetime.timedelta(minutes=70)).strftime('%H:%M'))")
python3 - "$CLAUDEGO_ROOT/config.json" "$LEAD_FROM" "$LEAD_TO" <<'EOF'
import json,sys
p,f,t=sys.argv[1:4]; c=json.load(open(p))
c["redline_windows"]=[{"from":f,"to":t,"queue_budget_tokens":1}]
c["redline_lead_min"]=15
json.dump(c,open(p,"w"),indent=2,ensure_ascii=False)
EOF
printf 'ok\n' > "$MOCK_DIR/plan"; echo 0 > "$MOCK_DIR/n"
: > "$MOCK_DIR/codex-calls.log"
"$BIN" add -dir "$PROJ" -title lead-claude -priority 2 "lead work" >/dev/null
"$BIN" add -dir "$PROJ2" -runner codex -title lead-codex -priority 2 "lead filler" >/dev/null
"$BIN" run -quiet
[ "$(cat "$MOCK_DIR/n")" = "0" ] && echo "  ✔ 缓冲期内 claude 任务不起跑" && pass=$((pass+1)) || { echo "  ✖ 缓冲期未生效"; fail=$((fail+1)); }
assert "codex 钉定任务缓冲期照跑" "one(title='lead-codex')['status']=='done' and one(title='lead-codex')['runner']=='codex'"
qout=$("$BIN" quota); echo "$qout" | grep -q "缓冲期生效中" && echo "  ✔ quota 显示缓冲状态" && pass=$((pass+1)) || { echo "  ✖ quota 未显示缓冲"; fail=$((fail+1)); }
python3 - "$CLAUDEGO_ROOT/config.json" <<'EOF'
import json,sys
p=sys.argv[1]; c=json.load(open(p)); c["redline_lead_min"]=0
json.dump(c,open(p,"w"),indent=2,ensure_ascii=False)
EOF
"$BIN" run -quiet
assert "关闭缓冲后 claude 任务正常执行" "one(title='lead-claude')['status']=='done'"

echo "== 场景21: 设计模型不降级（no_fallback_models）=="
python3 - "$CLAUDEGO_ROOT/config.json" <<'EOF'
import json,sys,time
p=sys.argv[1]; c=json.load(open(p))
json.dump({"until_epoch":int(time.time())+3600,"reason":"mock limit","set_at":"t"},
          open(p.replace("config.json","cooldown.json"),"w"))
json.dump(c,open(p,"w"),indent=2,ensure_ascii=False)
EOF
echo 0 > "$MOCK_DIR/n"
"$BIN" add -dir "$PROJ"  -model claude-fable-5 -title nf-fable -priority 2 "design work" >/dev/null
"$BIN" add -dir "$PROJ2" -model sonnet -title nf-sonnet -priority 2 "impl work" >/dev/null
"$BIN" run -quiet
assert "fable 设计卡冷却期不降级,继续排队" "one(title='nf-fable')['status']=='queued'"
assert "sonnet 卡照常 fallback 到 codex" "one(title='nf-sonnet')['status']=='done' and one(title='nf-sonnet')['runner']=='codex'"
rm -f "$CLAUDEGO_ROOT/cooldown.json"
printf 'ok\n' > "$MOCK_DIR/plan"; echo 0 > "$MOCK_DIR/n"
"$BIN" run -quiet
assert "冷却解除后 fable 卡由 claude 执行" "one(title='nf-fable')['status']=='done' and one(title='nf-fable').get('runner') is None"

echo "== 场景22: 协调链(coordinate 产出 coordinate,自愈式续排) =="
printf 'coordchain\nok\n' > "$MOCK_DIR/plan"; echo 0 > "$MOCK_DIR/n"
"$BIN" add -dir "$PROJ" -type coordinate -title chain-root -priority 2 "root planning" >/dev/null
"$BIN" run -quiet
assert "链根完成且产出下一张协调卡" "one(title='chain-root')['status']=='done' and one(title='chain-next')['type']=='coordinate'"
assert "产出的协调卡具备 emit 能力" "one(title='chain-next')['emit_tasks']==True"

echo "== 场景22: 只读审核卡与同仓写者并行（写者×2 仍串行由场景17保证）=="
printf 'slow\nslow\n' > "$MOCK_DIR/plan"; echo 0 > "$MOCK_DIR/n"
"$BIN" add -dir "$PROJ" -title par-writer -priority 2 "write work" >/dev/null
"$BIN" add -dir "$PROJ" -type design-review -title par-review -priority 2 "review work" >/dev/null
T0=$(python3 -c "import time;print(time.time())")
"$BIN" run -quiet
EL=$(python3 -c "import time,sys;print(time.time()-$T0)")
assert "同仓审核与写者都完成" "one(title='par-writer')['status']=='done' and one(title='par-review')['status']=='done'"
python3 -c "import sys;sys.exit(0 if $EL < 2.2 else 1)" && echo "  ✔ 只读审核并行不受目录互斥（${EL%.*}s < 2.2s）" && pass=$((pass+1)) || { echo "  ✖ 审核被目录互斥挡住（耗时 ${EL}s）"; fail=$((fail+1)); }

echo "== 场景23: emit 自造未知类型回退 sequence 并烘焙权限 =="
printf 'coord_badtype\n' > "$MOCK_DIR/plan"; echo 0 > "$MOCK_DIR/n"
"$BIN" plan -dir "$PROJ" -priority 9 -title bt-coord "自造类型分工" >/dev/null
"$BIN" run -quiet
assert "自造 batch 类型被回退为 sequence" "one(title='badtype-task')['type']=='sequence'"
assert "回退后烘焙了 sequence 权限（非空工具）" "len(one(title='badtype-task').get('allowed_tools') or [])>0 and one(title='badtype-task').get('permission_mode')=='acceptEdits'"

echo "== 场景24: 远端执行器（SSH → 远端 codex；prompt 走 stdin + marker 回捕；非零退出不误判）=="
python3 - "$CLAUDEGO_ROOT/config.json" "$PWD/test/mock-ssh.sh" <<'EOF'
import json,sys
p,mock=sys.argv[1],sys.argv[2]
c=json.load(open(p))
c["ssh_bin"]=mock
c["remote_hosts"]={"rhost":{"sandbox":"danger-full-access","tmp_dir":"/tmp","shell":"posix"}}
json.dump(c,open(p,"w"),indent=2,ensure_ascii=False)
EOF
chmod +x test/mock-ssh.sh
: > "$MOCK_DIR/ssh-calls.log"
"$BIN" add -dir "D:/remote/work" -host rhost -title r-remote -priority 8 "remote work step" >/dev/null
"$BIN" run -quiet
assert "远端任务完成且 runner=remote:rhost" "one(title='r-remote')['status']=='done' and one(title='r-remote')['runner']=='remote:rhost'"
grep -q "remote work step" "$MOCK_DIR/ssh-calls.log" && echo "  ✔ prompt 经 ssh stdin 灌入远端" && pass=$((pass+1)) || { echo "  ✖ prompt 未经 stdin 灌入"; fail=$((fail+1)); }
assert "结果取 marker 之后内容（Windows codex 非零退出不算失败）" "'remote step done' in (one(title='r-remote').get('last_summary') or '')"

echo
echo "结果: $pass 通过, $fail 失败"
[ "$fail" -eq 0 ]
