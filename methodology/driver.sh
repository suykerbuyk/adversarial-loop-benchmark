#!/usr/bin/env bash
# Chair-owned driver for the imp/adv adversarial-loop model benchmark.
# Harness only. Runs the frozen briefs and the mechanical stop rule.
# Does not adjudicate. Does not rewrite a cell.
#
# Isolation (measured 2026-10-30, after a series was invalidated):
#   claude: --bare --disable-slash-commands --strict-mcp-config --no-session-persistence
#           A cell without --bare received the vibe-palace skill listing and ran `vp hook`.
#   grok:   GROK_HOME with compat.*.skills/mcps/hooks = false, plus --disable-web-search
#           --no-subagents. The default ~/.grok config enables mcp_servers.vibe-palace.
#   codex:  --ignore-user-config --ignore-rules --ephemeral, prompt on stdin.
#           ~/.codex/config.toml enables mcp_servers.vibe-palace. A prompt argument
#           plus a closed stdin makes codex append stdin as a <stdin> block.
#
# A turn that exits non-zero, times out, or emits unusable usage JSON is
# did_not_finish. It is never scored as a converged cell.
set -uo pipefail

ROOT=/home/user/adv-imp-benchmark-runs
SRC=/home/user/adv-imp-benchmark
ISO="$ROOT/runtime/grok-home"
RUNDIR="$ROOT/2026-10-30-claude-codex-grok"
RESULTS="$RUNDIR/results.jsonl"
LOG="$RUNDIR/driver.log"
CEILING=5.00
GROK_CEILING=10.00   # per-operator: cells involving grok (imp or adv == G) get $10 — grok-4.7's 256K window drives iteration-heavy, pricier reviews
ROUND_TOKEN_CAP=1000000
CELL_TOKEN_CAP=4000000
SPECSRC="$SRC/spec/config-resolver-cell-facing.md"
PTSRC="$SRC/public-tests/config/resolve_public_test.go"
HELDOUT="$SRC/held-out/config/generalization_test.go"
CLAUDE_MODEL=claude-opus-5-5
GROK_MODEL=grok-4.7
CODEX_MODEL=gpt-6-luna
TURN_TIMEOUT=1800
# go 1.25 in the cell module is the language line. The host toolchain is 1.27.2.
# local refuses a download. testing/synctest needs that host toolchain.
export GOTOOLCHAIN=local

mkdir -p "$RUNDIR" "$ISO"
touch "$RESULTS"
log(){ echo "[$(date '+%F %T')] $*" | tee -a "$LOG"; }
addf(){ awk -v a="$1" -v b="$2" 'BEGIN{printf "%.6f", a+b}'; }
addi(){ awk -v a="${1:-0}" -v b="${2:-0}" 'BEGIN{printf "%d", a+b}'; }
cell_ceiling(){ if [ "${imp:-}" = G ] || [ "${adv:-}" = G ]; then printf '%s' "$GROK_CEILING"; else printf '%s' "$CEILING"; fi; }
over_cost(){ awk -v c="$1" -v cap="$(cell_ceiling)" 'BEGIN{exit !(c+0>cap)}'; }
over_tok(){ awk -v c="$1" -v cap="$2" 'BEGIN{exit !(c+0>cap)}'; }

ensure_grok_home(){
  # Fresh-enough home: never copy ~/.grok. Auth is read from the user keychain
  # / API env, not from this file. compat flags disable Claude-imported skills
  # and hooks (measured: inspect marks them disabled; system_prompt has no
  # vibe-palace / herdr / skill catalog).
  cat > "$ISO/config.toml" <<'EOF'
[marketplace]
default_skills_installs_purged = true

[compat.claude]
skills = false
rules = false
agents = false
mcps = false
hooks = false
sessions = false

[compat.cursor]
skills = false
rules = false
agents = false
mcps = false
hooks = false
sessions = false
EOF
}

# Strip the chair's session identity so a child CLI cannot inherit it.
cell_env(){
  env -u HERDR_ENV -u HERDR_PANE_ID -u HERDR_TAB_ID -u HERDR_WORKSPACE_ID \
      -u HERDR_SOCKET_PATH -u HERDR_SESSION -u HERDR_BIN_PATH \
      -u CLAUDECODE -u CLAUDE_CODE_ENTRYPOINT -u CLAUDE_CODE_SESSION_ID \
      -u CLAUDE_CODE_CHILD_SESSION -u CLAUDE_CODE_BRIDGE_SESSION_ID \
      -u CLAUDE_CODE_MESSAGING_SOCKET -u CLAUDE_CODE_MESSAGING_TOKEN \
      -u CLAUDE_CODE_EXECPATH -u CLAUDE_CODE_SESSION_ATTENDED \
      -u CLAUDE_VAULT \
      "$@"
}

# run_turn KIND BRIEFFILE OUTFILE CWD
# stdout of the function is unused. Exit status is the CLI's (124 on timeout).
run_turn(){
  local kind=$1 brief=$2 out=$3 cwd=$4
  case $kind in
    C) cell_env timeout "$TURN_TIMEOUT" claude -p "$(cat "$brief")" \
         --output-format json \
         --bare --disable-slash-commands --no-session-persistence \
         --strict-mcp-config --mcp-config '{"mcpServers":{}}' \
         --model "$CLAUDE_MODEL" --dangerously-skip-permissions \
         >"$out" 2>"$out.err" ;;
    G) ( cd "$cwd" && cell_env timeout "$TURN_TIMEOUT" \
           env GROK_HOME="$ISO" grok -p "$(cat "$brief")" \
           --output-format json -m "$GROK_MODEL" \
           --always-approve --disable-web-search --no-subagents --verbatim \
         ) >"$out" 2>"$out.err" ;;
    X) ( cd "$cwd" && cell_env timeout "$TURN_TIMEOUT" \
           codex exec --json --skip-git-repo-check \
           --ignore-user-config --ignore-rules --ephemeral \
           --sandbox workspace-write -m "$CODEX_MODEL" \
           -c 'mcp_servers={}' \
           - <"$brief" \
         ) >"$out" 2>"$out.err" ;;
  esac
}

# Echo "cost tokens fresh model ok|bad".
# tokens is the recorded total (includes cache-read). fresh is what the cap
# compares. Cache-read is a replay of context the model already paid to write,
# not new work. grok-4.7's 256K window makes total_tokens a cache counter.
turn_usage(){
  local kind=$1 out=$2
  python3 - "$kind" "$out" <<'PY'
import json, sys
kind, path = sys.argv[1], sys.argv[2]
def n(u, *keys):
    for k in keys:
        if k in u and u[k] is not None:
            return int(u[k])
    return 0
try:
    raw = open(path).read()
except OSError:
    print("0 0 0  bad")
    sys.exit(0)
if not raw.strip():
    print("0 0 0  bad")
    sys.exit(0)
try:
    if kind == "X":
        last = None
        for line in raw.splitlines():
            o = json.loads(line)
            if o.get("type") == "turn.completed":
                last = o
        if not last or "usage" not in last:
            print("0 0 0  bad")
            sys.exit(0)
        u = last["usage"]
        # Recorded total includes cache. Cap excludes cached_input_tokens.
        recorded = (n(u, "input_tokens") + n(u, "output_tokens")
                    + n(u, "reasoning_output_tokens") + n(u, "cached_input_tokens"))
        fresh = n(u, "input_tokens") + n(u, "output_tokens") + n(u, "reasoning_output_tokens")
        print(f"0.000000 {recorded} {fresh} gpt-6-luna ok")
        sys.exit(0)
    d = json.loads(raw)
except Exception:
    print("0 0 0  bad")
    sys.exit(0)
if kind == "C":
    if d.get("is_error") or d.get("subtype") not in (None, "success"):
        print("0 0 0  bad")
        sys.exit(0)
    u = d.get("usage") or {}
    details = u.get("output_tokens_details") or {}
    thinking = n(details, "thinking_tokens")
    recorded = (n(u, "input_tokens") + n(u, "output_tokens")
                + n(u, "cache_read_input_tokens") + n(u, "cache_creation_input_tokens")
                + thinking)
    # Cap: fresh only. cache_read_input_tokens excluded.
    fresh = (n(u, "input_tokens") + n(u, "output_tokens")
             + n(u, "cache_creation_input_tokens") + thinking)
    cost = float(d.get("total_cost_usd") or 0)
    mu = d.get("modelUsage") or {}
    model = next(iter(mu), "") or "claude-opus-5-5"
elif kind == "G":
    u = d.get("usage") or {}
    if "input_tokens" not in u and "total_tokens" not in u:
        print("0 0 0  bad")
        sys.exit(0)
    # Recorded total is the vendor total, which includes cache-read.
    recorded = n(u, "total_tokens")
    if recorded == 0:
        recorded = (n(u, "input_tokens") + n(u, "output_tokens") + n(u, "reasoning_tokens")
                    + n(u, "cache_read_input_tokens", "cacheReadInputTokens"))
    # Cap: fresh components only. Do not use total_tokens.
    fresh = n(u, "input_tokens") + n(u, "output_tokens") + n(u, "reasoning_tokens")
    cost = float(d.get("total_cost_usd") or 0)
    model = "grok-4.7"
else:
    print("0 0 0  bad")
    sys.exit(0)
print(f"{cost:.6f} {recorded} {fresh} {model} ok")
PY
}

# A findings file converges only when it exists, has at least one tagged line,
# and no tagged line is blocker or major. Prose that merely says "no major"
# does not count. A missing file does not count.
converged(){
  local f=$1
  [ -f "$f" ] || return 1
  python3 - "$f" <<'PY'
import re, sys
text = open(sys.argv[1], encoding="utf-8", errors="replace").read()
tags = re.findall(r"(?im)^[ \t]*(?:\*\*)?(blocker|major|minor|nit)(?:\*\*)?[ \t]*[:\-]", text)
if not tags:
    sys.exit(1)
if any(t.lower() in ("blocker", "major") for t in tags):
    sys.exit(1)
sys.exit(0)
PY
}

b_imp_plan(){ cat <<EOF
ROLE: implementor. Read the task spec at $SPEC and the public tests at $PT. If prior
adversary findings exist at $PF, read them and address each (fix the plan, or rebut the
finding in writing). Produce a complete implementation plan: the public API, the internal data
model, how each stated rule is satisfied, and the test approach. Write the plan to $PLAN. If you
are responding to findings, also write a point-by-point response (fixed / rebutted-with-reason) to
$PR. Do not implement anything. Write nothing else.
EOF
}
# Phase-2 plan. The v1 snapshot is in $WT. Phase-1 plan, findings, and rebuttal
# are not in this brief and are not in the cell files this turn can see.
b_imp_plan_extend(){ cat <<EOF
ROLE: implementor. Read the v2 amendment at $SPEC, the v2 public tests at $PT, and the
existing v1 code in $WT. If prior adversary findings exist at $PF, read them and address each.
Produce a plan for the amendment: what changes, what stays, and how each v2 rule is satisfied.
Write the plan to $PLAN. If you are responding to findings, also write a point-by-point response to
$PR. Do not implement anything. Write nothing else.
EOF
}
b_adv_plan(){ cat <<EOF
ROLE: adversarial reviewer. Read the task spec at $SPEC, the public tests at $PT, the
implementor's plan at $PLAN, and any prior rebuttal at $PR. Find defects, omissions,
and architectural weaknesses that would make the implementation fail a stated rule or be hard to
maintain over years. Re-derive every claim from the spec and tests; do not trust the plan's
self-description. Tag every finding exactly one of: blocker, major, minor, nit. Write the findings
to $PF, one per line, each prefixed with its tag. If no blocker or major issue remains,
state that explicitly.
EOF
}
b_imp_impl(){ cat <<EOF
ROLE: implementor. You begin with empty context. Read ONLY the task spec at $SPEC, the public
tests at $PT, and the approved plan at $PLAN. Implement the Go package in $WT so
that \`go build\`, \`go vet\`, and \`go test ./...\` all pass. If prior code-review findings exist at $CF, address each and write a point-by-point response to $CR. Write only code
in $WT and the rebuttal file. Do not modify the public test file.
EOF
}
b_adv_code(){ cat <<EOF
ROLE: adversarial reviewer. Read the task spec at $SPEC, the public tests at $PT, the
implementor's code in $WT, and any prior rebuttal at $CR. Build and test it
yourself; re-derive every claim from source. Find defects, missing cases, race or resource issues,
weak error handling, and maintainability/extensibility problems. Tag every finding exactly one of:
blocker, major, minor, nit. Write the findings to $CF, one per line with its tag. If no
blocker or major issue remains, state that explicitly.
EOF
}

seed_worktree(){
  seed_instrument
}

grade_cell(){
  local build=fail vet=fail public=fail gen=fail race=fail api=fail loc=0
  if ( cd "$WT" && go build ./... ) >/dev/null 2>"$cell/grade-build.err"; then build=pass; fi
  if ( cd "$WT" && go vet ./... ) >/dev/null 2>"$cell/grade-vet.err"; then vet=pass; fi
  if ( cd "$WT" && go test ./... ) >/dev/null 2>"$cell/grade-public.err"; then public=pass; fi
  cp "$HELDOUT" "$WT/generalization_test.go"
  if ( cd "$WT" && go test ./... ) >/dev/null 2>"$cell/grade-gen.err"; then gen=pass; fi
  rm -f "$WT/generalization_test.go"
  if ( cd "$WT" && go test -race ./... ) >/dev/null 2>"$cell/grade-race.err"; then race=pass; fi
  # API: the three names must be the package's exported declarations, not a
  # substring of a doc comment. go doc -all lists them as lines.
  if ( cd "$WT" && go doc -all . ) >"$cell/grade-api.txt" 2>/dev/null; then
    if grep -qE '^func Resolve\(' "$cell/grade-api.txt" \
       && grep -qE '^type Config struct' "$cell/grade-api.txt" \
       && grep -qE '^var ErrInvalidLogLevel' "$cell/grade-api.txt"; then
      api=pass
    fi
  fi
  loc=$(find "$WT" -maxdepth 1 -name '*.go' ! -name '*_test.go' -print0 \
    | xargs -0 wc -l 2>/dev/null \
    | awk 'NF>=2 && $2 != "total" {s+=$1} END{print s+0}')
  printf '%s %s %s %s %s %s %s\n' "$build" "$vet" "$public" "$gen" "$race" "$api" "$loc"
}

record_row(){
  local grade=${1:-}
  jq -cn \
    --arg imp "$imp" --arg adv "$adv" --argjson rep "$rep" \
    --argjson prr "$pr_rounds" --argjson pc "$pc" \
    --argjson crr "$cr_rounds" --argjson cc "$cc" \
    --arg grade "$grade" \
    --argjson cost "$cost" --argjson tokens "$tokens" \
    --argjson tok_imp "$tok_imp" --argjson tok_adv "$tok_adv" \
    --argjson fresh "$fresh" --argjson fresh_imp "$fresh_imp" --argjson fresh_adv "$fresh_adv" \
    --arg model_imp "$model_imp" --arg model_adv "$model_adv" \
    --argjson dnf "$dnf" --arg reason "$reason" \
    --arg protocol "isolated-2026-10-30" \
    --arg profile "$PROFILE" \
    '{imp:$imp,adv:$adv,rep:$rep,profile:$profile,plan_rounds:$prr,plan_converged:$pc,code_rounds:$crr,code_converged:$cc,grade:$grade,measurable_cost_usd:$cost,tokens:$tokens,tokens_imp:$tok_imp,tokens_adv:$tok_adv,fresh_tokens:$fresh,fresh_imp:$fresh_imp,fresh_adv:$fresh_adv,model_imp:$model_imp,model_adv:$model_adv,did_not_finish:$dnf,reason:$reason,protocol:$protocol}' \
    >> "$RESULTS"
}

record_walcodec_row(){
  local grade=$1
  jq -cn \
    --arg imp "$imp" --arg adv "$adv" --argjson rep "$rep" \
    --argjson p1 "$p1_rounds" --argjson c1 "$c1_rounds" \
    --argjson p1c "$p1_conv" --argjson c1c "$c1_conv" \
    --argjson p2 "$p2_rounds" --argjson c2 "$c2_rounds" \
    --argjson p2c "$p2_conv" --argjson c2c "$c2_conv" \
    --argjson cost1 "$cost1" --argjson tok1 "$tok1" \
    --argjson cost2 "$cost2" --argjson tok2 "$tok2" \
    --argjson cost "$cost" --argjson tokens "$tokens" \
    --argjson fresh "$fresh" --argjson fresh_imp "$fresh_imp" --argjson fresh_adv "$fresh_adv" \
    --arg grade "$grade" \
    --argjson dnf "$dnf" --arg reason "$reason" \
    --arg protocol "isolated-2026-10-30" \
    '{imp:$imp,adv:$adv,rep:$rep,profile:"walcodec",plan1_rounds:$p1,plan1_converged:$p1c,code1_rounds:$c1,code1_converged:$c1c,plan2_rounds:$p2,plan2_converged:$p2c,code2_rounds:$c2,code2_converged:$c2c,cost1:$cost1,tokens1:$tok1,cost2:$cost2,tokens2:$tok2,measurable_cost_usd:$cost,tokens:$tokens,fresh_tokens:$fresh,fresh_imp:$fresh_imp,fresh_adv:$fresh_adv,grade:$grade,did_not_finish:$dnf,reason:$reason,protocol:$protocol,headline:"phase2_convergence+v1_regression+api"}' \
    >> "$RESULTS"
}

# One turn. Sets dnf/reason and updates cost/tokens on failure or cap.
# Returns 0 to continue, 1 to abort the cell.
do_turn(){
  local role=$1 kind=$2 brief=$3 out=$4 cwd=$5 phase=$6
  local rc usage cost_i tok_i fresh_i model_i status
  run_turn "$kind" "$brief" "$out" "$cwd"
  rc=$?
  if [ "$rc" -eq 124 ]; then
    dnf=true; reason="timeout at $phase"; return 1
  fi
  if [ "$rc" -ne 0 ]; then
    dnf=true; reason="exit $rc at $phase"; return 1
  fi
  usage=$(turn_usage "$kind" "$out") || usage="0 0 0  bad"
  read -r cost_i tok_i fresh_i model_i status <<<"$usage"
  if [ "$status" != "ok" ]; then
    dnf=true; reason="unusable usage JSON at $phase"; return 1
  fi
  cost=$(addf "$cost" "$cost_i")
  # Recorded totals keep cache-read. The cap does not.
  tokens=$(addi "$tokens" "$tok_i")
  fresh=$(addi "$fresh" "$fresh_i")
  if [ "$role" = imp ]; then
    tok_imp=$(addi "$tok_imp" "$tok_i")
    fresh_imp=$(addi "$fresh_imp" "$fresh_i")
    model_imp=$model_i
  else
    tok_adv=$(addi "$tok_adv" "$tok_i")
    fresh_adv=$(addi "$fresh_adv" "$fresh_i")
    model_adv=$model_i
  fi
  if over_tok "$fresh_i" "$ROUND_TOKEN_CAP"; then
    dnf=true; reason="round fresh tokens $fresh_i > $ROUND_TOKEN_CAP at $phase"; return 1
  fi
  if over_tok "$fresh" "$CELL_TOKEN_CAP"; then
    dnf=true; reason="cell fresh tokens $fresh > $CELL_TOKEN_CAP at $phase"; return 1
  fi
  if [ "$kind" != X ] && over_cost "$cost"; then
    dnf=true; reason="cost>\$$(cell_ceiling) at $phase"; return 1
  fi
  return 0
}

cell_already_done(){
  local imp=$1 adv=$2 rep=$3
  python3 - "$RESULTS" "$imp" "$adv" "$rep" "$PROFILE" <<'PY'
import json, sys
path, imp, adv, rep, profile = sys.argv[1:]
rep = int(rep)
try:
    lines = open(path)
except OSError:
    sys.exit(1)
for line in lines:
    line=line.strip()
    if not line:
        continue
    o=json.loads(line)
    if (o.get("protocol")=="isolated-2026-10-30" and o.get("imp")==imp
            and o.get("adv")==adv and o.get("rep")==rep
            and o.get("profile", "resolver")==profile
            and not o.get("did_not_finish")):
        sys.exit(0)
sys.exit(1)
PY
}

run_cell(){
  local imp=$1 adv=$2 rep=$3
  if [ "$PROFILE" = walcodec ]; then
    run_walcodec_cell "$imp" "$adv" "$rep"
    return
  fi
  if cell_already_done "$imp" "$adv" "$rep"; then
    log "SKIP $imp->$adv rep$rep (already recorded under isolated protocol)"
    return
  fi

  local cell="$RUNDIR/${imp}to${adv}/rep${rep}"
  rm -rf "$cell"
  mkdir -p "$cell/plan" "$cell/instrument" "$cell/worktree" "$cell/turns"
  cp "$SPECSRC" "$cell/spec.md"
  cp "$PTSRC" "$cell/instrument/resolve_public_test.go"
  SPEC="$cell/spec.md"
  PT="$cell/instrument/resolve_public_test.go"
  PLAN="$cell/plan/plan.md"
  PF="$cell/plan/plan_findings.md"
  PR="$cell/plan/plan_rebuttal.md"
  WT="$cell/worktree"
  CF="$cell/code_findings.md"
  CR="$cell/code_rebuttal.md"
  seed_worktree

  local cost=0 tokens=0 fresh=0 fresh_imp=0 fresh_adv=0 tok_imp=0 tok_adv=0
  local dnf=false reason="" pr_rounds=0 pc=false cr_rounds=0 cc=false
  local model_imp="" model_adv=""
  log "START $imp->$adv rep$rep"

  for r in 1 2 3; do
    pr_rounds=$r
    b_imp_plan > "$cell/plan/brief_imp_plan_r$r.txt"
    do_turn imp "$imp" "$cell/plan/brief_imp_plan_r$r.txt" "$cell/turns/p${r}_imp.json" "$cell" "plan-imp r$r" || break
    b_adv_plan > "$cell/plan/brief_adv_plan_r$r.txt"
    do_turn adv "$adv" "$cell/plan/brief_adv_plan_r$r.txt" "$cell/turns/p${r}_adv.json" "$cell" "plan-adv r$r" || break
    if converged "$PF"; then pc=true; break; fi
  done

  seed_worktree
  if [ "$dnf" = false ]; then
    for r in 1 2 3; do
      cr_rounds=$r
      b_imp_impl > "$cell/turns/brief_imp_impl_r$r.txt"
      do_turn imp "$imp" "$cell/turns/brief_imp_impl_r$r.txt" "$cell/turns/c${r}_imp.json" "$WT" "code-imp r$r" || break
      b_adv_code > "$cell/turns/brief_adv_code_r$r.txt"
      do_turn adv "$adv" "$cell/turns/brief_adv_code_r$r.txt" "$cell/turns/c${r}_adv.json" "$cell" "code-adv r$r" || break
      if converged "$CF"; then cc=true; break; fi
    done
  fi

  local grade
  grade=$(grade_cell)
  record_grade "$grade"
  log "DONE  $imp->$adv rep$rep profile=$PROFILE plan=$pr_rounds(conv=$pc) code=$cr_rounds(conv=$cc) cost=\$$cost tokens=$tokens dnf=$dnf $reason grade=$grade"
}

# One plan→code loop. Uses the current SPEC/PT. Snapshots nothing.
# Sets p1_* or p2_* via the phase argument.
run_phase_loop(){
  local phase=$1
  local pr=0 pc=false cr=0 cc=false
  local cost_before=$cost tok_before=$tokens
  PHASE=$phase
  seed_instrument
  for r in 1 2 3; do
    pr=$r
    if [ "$phase" = 2 ]; then
      b_imp_plan_extend > "$cell/plan/brief_imp_plan_p${phase}_r$r.txt"
    else
      b_imp_plan > "$cell/plan/brief_imp_plan_p${phase}_r$r.txt"
    fi
    do_turn imp "$imp" "$cell/plan/brief_imp_plan_p${phase}_r$r.txt" "$cell/turns/p${phase}_${r}_imp.json" "$cell" "p${phase}-plan-imp r$r" || break
    b_adv_plan > "$cell/plan/brief_adv_plan_p${phase}_r$r.txt"
    do_turn adv "$adv" "$cell/plan/brief_adv_plan_p${phase}_r$r.txt" "$cell/turns/p${phase}_${r}_adv.json" "$cell" "p${phase}-plan-adv r$r" || break
    if converged "$PF"; then pc=true; break; fi
  done
  seed_instrument
  if [ "$dnf" = false ]; then
    for r in 1 2 3; do
      cr=$r
      b_imp_impl > "$cell/turns/brief_imp_impl_p${phase}_r$r.txt"
      do_turn imp "$imp" "$cell/turns/brief_imp_impl_p${phase}_r$r.txt" "$cell/turns/c${phase}_${r}_imp.json" "$WT" "p${phase}-code-imp r$r" || break
      b_adv_code > "$cell/turns/brief_adv_code_p${phase}_r$r.txt"
      do_turn adv "$adv" "$cell/turns/brief_adv_code_p${phase}_r$r.txt" "$cell/turns/c${phase}_${r}_adv.json" "$cell" "p${phase}-code-adv r$r" || break
      if converged "$CF"; then cc=true; break; fi
    done
  fi
  if [ "$phase" = 1 ]; then
    p1_rounds=$pr; p1_conv=$pc; c1_rounds=$cr; c1_conv=$cc
    cost1=$(addf "$cost" "-$cost_before")
    tok1=$(addi "$tokens" "-$tok_before")
  else
    p2_rounds=$pr; p2_conv=$pc; c2_rounds=$cr; c2_conv=$cc
    cost2=$(addf "$cost" "-$cost_before")
    tok2=$(addi "$tokens" "-$tok_before")
  fi
}

run_walcodec_cell(){
  local imp=$1 adv=$2 rep=$3
  if cell_already_done "$imp" "$adv" "$rep"; then
    log "SKIP $imp->$adv rep$rep (already recorded under isolated protocol)"
    return
  fi
  local cell="$RUNDIR/${imp}to${adv}/rep${rep}"
  rm -rf "$cell"
  mkdir -p "$cell/plan" "$cell/instrument" "$cell/worktree" "$cell/turns" "$cell/snapshot-v1" "$cell/snapshot-v2"
  WT="$cell/worktree"
  PLAN="$cell/plan/plan.md"
  PF="$cell/plan/plan_findings.md"
  PR="$cell/plan/plan_rebuttal.md"
  CF="$cell/code_findings.md"
  CR="$cell/code_rebuttal.md"
  local cost=0 tokens=0 fresh=0 fresh_imp=0 fresh_adv=0 tok_imp=0 tok_adv=0
  local dnf=false reason=""
  local model_imp="" model_adv=""
  local p1_rounds=0 p1_conv=false c1_rounds=0 c1_conv=false
  local p2_rounds=0 p2_conv=false c2_rounds=0 c2_conv=false
  local cost1=0 tok1=0 cost2=0 tok2=0
  log "START $imp->$adv rep$rep profile=walcodec phase=1"

  # Phase 1. Spec and tests are v1 only.
  SPEC="$cell/spec-v1.md"
  PT="$cell/instrument/walcodec_public_test.go"
  cp "$SPECSRC" "$SPEC"
  cp "$PTSRC" "$PT"
  PHASE=1
  run_phase_loop 1
  # Snapshot implementation only. Plan, findings, and rebuttal stay out of it.
  find "$WT" -maxdepth 1 -name '*.go' ! -name '*_test.go' -exec cp {} "$cell/snapshot-v1/" \;
  cp "$WT/go.mod" "$cell/snapshot-v1/" 2>/dev/null || true

  if [ "$dnf" = false ]; then
    log "PHASE2 $imp->$adv rep$rep (plan turn sees snapshot+amendment+v2 tests only)"
    # Phase-2 plan must not see phase-1 plan, findings, or rebuttal.
    rm -f "$PLAN" "$PF" "$PR" "$CF" "$CR"
    SPEC="$cell/amendment.md"
    PT="$cell/instrument/walcodec_v2_public_test.go"
    cp "$AMENDSRC" "$SPEC"
    cp "$PT2SRC" "$PT"
    # The plan turn's worktree is the v1 snapshot, not the phase-1 chatter.
    rm -rf "$WT"
    mkdir -p "$WT"
    cp -a "$cell/snapshot-v1/." "$WT/"
    printf 'module walcodec\n\ngo 1.25\n' > "$WT/go.mod"
    PHASE=2
    run_phase_loop 2
    find "$WT" -maxdepth 1 -name '*.go' ! -name '*_test.go' -exec cp {} "$cell/snapshot-v2/" \;
    cp "$WT/go.mod" "$cell/snapshot-v2/" 2>/dev/null || true
  fi

  local grade
  grade=$(grade_walcodec)
  record_walcodec_row "$grade"
  log "DONE  $imp->$adv rep$rep profile=walcodec p1=$p1_rounds(conv=$p1_conv)/$c1_rounds(conv=$c1_conv) p2=$p2_rounds(conv=$p2_conv)/$c2_rounds(conv=$c2_conv) cost1=\$$cost1 cost2=\$$cost2 dnf=$dnf $reason grade=$grade"
}

run_matrix(){
  for rep in 1 2 3; do run_cell C C "$rep"; done
  for rep in 1 2 3; do run_cell C G "$rep"; done
  for rep in 1 2 3; do run_cell C X "$rep"; done
  for rep in 1 2 3; do run_cell G C "$rep"; done
  for rep in 1 2 3; do run_cell G G "$rep"; done
  for rep in 1 2 3; do run_cell G X "$rep"; done
  for rep in 1 2 3; do run_cell X C "$rep"; done
  for rep in 1 2 3; do run_cell X G "$rep"; done
  for rep in 1 2 3; do run_cell X X "$rep"; done
}

# Calibration is plumbing, not a matrix replicate. It lives in its own dir so
# a later C→G rep1 is a fresh cell, not a resume of this one.
# A stale did_not_finish row must not block a fresh calibration: the cell dir
# is removed before the skip check sees it. The jsonl is kept as history.
# Skip already ignores did_not_finish rows.
run_calibration(){
  local series=$1
  RUNDIR="$ROOT/$series/cal-C-to-G-bare"
  RESULTS="$RUNDIR/results.jsonl"
  LOG="$RUNDIR/driver.log"
  mkdir -p "$RUNDIR"
  touch "$RESULTS"
  rm -rf "$RUNDIR/CtoG"
  local before after
  before=$(find /home/user/vibe-palace-vault/Projects/vibe-palace/sessions -name '2026-10-30-*.md' | wc -l)
  log "==== CALIBRATION START ($(date)) profile=$PROFILE session_notes_before=$before ===="
  run_cell C G 1
  after=$(find /home/user/vibe-palace-vault/Projects/vibe-palace/sessions -name '2026-10-30-*.md' | wc -l)
  log "==== CALIBRATION END session_notes_before=$before after=$after ===="
  if [ "$after" -gt "$before" ]; then
    log "ABORT: vibe-palace session-note count rose during calibration ($before -> $after)"
    return 1
  fi
  python3 - "$RESULTS" <<'PY'
import json, sys
rows=[json.loads(l) for l in open(sys.argv[1]) if l.strip()]
if not rows or rows[-1].get("did_not_finish"):
    sys.exit(1)
sys.exit(0)
PY
}

# --- instrument profiles ---------------------------------------------------
# Isolation, briefs, stop rule, and tripwires are shared. A profile owns the
# spec paths, the worktree seed, and the grade. The resolver profile is the
# completed series and is not a launch target.

use_resolver(){
  PROFILE=resolver
  SERIES=2026-10-30-claude-codex-grok
  SPECSRC="$SRC/spec/config-resolver-cell-facing.md"
  PTSRC="$SRC/public-tests/config/resolve_public_test.go"
  HELDOUT="$SRC/held-out/config/generalization_test.go"
}

use_blockcache(){
  PROFILE=blockcache
  SERIES=2026-10-30-claude-codex-grok-blockcache
  SPECSRC="$SRC/blockcache/spec/blockcache-cell-facing.md"
  PTSRC="$SRC/blockcache/public-tests/blockcache/blockcache_public_test.go"
  HELDOUT="$SRC/blockcache/held-out/blockcache/generalization_test.go"
  LEAKSRC="$SRC/blockcache/harness/leakcheck/leak_test.go"
  STRESSSRC="$SRC/blockcache/harness/stress/main.go"
}

use_walcodec(){
  PROFILE=walcodec
  SERIES=2026-10-30-claude-codex-grok-walcodec
  SPECSRC="$SRC/walcodec/spec/walcodec-v1-cell-facing.md"
  AMENDSRC="$SRC/walcodec/spec/walcodec-v2-amendment.md"
  PTSRC="$SRC/walcodec/public-tests/v1/walcodec_public_test.go"
  PT2SRC="$SRC/walcodec/public-tests/v2/walcodec_v2_public_test.go"
  HELDOUT="$SRC/walcodec/held-out/walcodec/generalization_test.go"
  DIFFSRC="$SRC/walcodec/harness/diff.go"
}

bind_series(){
  RUNDIR="$ROOT/$SERIES"
  RESULTS="$RUNDIR/results.jsonl"
  LOG="$RUNDIR/driver.log"
  mkdir -p "$RUNDIR"
  touch "$RESULTS"
}

seed_resolver(){
  rm -f "$WT"/*.go
  printf 'module config\n\ngo 1.25\n' > "$WT/go.mod"
  cp "$PTSRC" "$WT/resolve_public_test.go"
}

seed_blockcache(){
  rm -f "$WT"/*.go
  printf 'module blockcache\n\ngo 1.25\n' > "$WT/go.mod"
  cp "$PTSRC" "$WT/blockcache_public_test.go"
}

seed_walcodec(){
  rm -f "$WT"/*.go
  printf 'module walcodec\n\ngo 1.25\n' > "$WT/go.mod"
  cp "$PTSRC" "$WT/walcodec_public_test.go"
  if [ "${PHASE:-1}" = 2 ]; then
    cp "$PT2SRC" "$WT/walcodec_v2_public_test.go"
  else
    rm -f "$WT/walcodec_v2_public_test.go"
  fi
}

seed_instrument(){
  case "$PROFILE" in
    resolver) seed_resolver ;;
    blockcache) seed_blockcache ;;
    walcodec) seed_walcodec ;;
  esac
}

grade_go(){
  local label=$1
  shift
  if ( cd "$WT" && "$@" ) >/dev/null 2>"$cell/grade-$label.err"; then
    echo pass
  else
    echo fail
  fi
}

# Exported names from go doc -all, one per line, declarations only.
api_names(){
  ( cd "$WT" && go doc -all . ) 2>/dev/null | awk '
    /^func / { print $2; next }
    /^type / { print $2; next }
    /^var / { print $2; next }
    /^const / { print $2; next }
  ' | sed 's/[(].*//; s/[[].*//'
}

grade_resolver(){
  local build vet public gen race api loc
  build=$(grade_go build go build ./...)
  vet=$(grade_go vet go vet ./...)
  public=$(grade_go public go test ./...)
  cp "$HELDOUT" "$WT/generalization_test.go"
  gen=$(grade_go gen go test ./...)
  rm -f "$WT/generalization_test.go"
  race=$(grade_go race go test -race ./...)
  api=fail
  if ( cd "$WT" && go doc -all . ) >"$cell/grade-api.txt" 2>/dev/null; then
    if grep -qE '^func Resolve\(' "$cell/grade-api.txt" \
       && grep -qE '^type Config struct' "$cell/grade-api.txt" \
       && grep -qE '^var ErrInvalidLogLevel' "$cell/grade-api.txt"; then
      api=pass
    fi
  fi
  loc=$(find "$WT" -maxdepth 1 -name '*.go' ! -name '*_test.go' -print0 \
    | xargs -0 wc -l 2>/dev/null \
    | awk 'NF>=2 && $2 != "total" {s+=$1} END{print s+0}')
  printf '%s %s %s %s %s %s %s\n' "$build" "$vet" "$public" "$gen" "$race" "$api" "$loc"
}

grade_blockcache(){
  local build vet public race_public gen gen_race leak api loc stress=advisory nproc=advisory
  build=$(grade_go build go build ./...)
  vet=$(grade_go vet go vet ./...)
  public=$(grade_go public go test ./...)
  race_public=$(grade_go race-public go test -race ./...)
  cp "$HELDOUT" "$WT/generalization_test.go"
  gen=$(grade_go gen go test -count=1 -run 'TestGen' ./...)
  gen_race=$(grade_go gen-race go test -race -count=1 -run 'TestGen' ./...)
  rm -f "$WT/generalization_test.go"
  # Hard leak gate: Close returns within 5s while a Loader is in flight.
  # The harness file is chair-only and is removed before the row is recorded.
  cp "$LEAKSRC" "$WT/leak_test.go"
  leak=$(grade_go leak go test -count=1 -timeout 30s -run TestLeakCloseReturnsWithinDeadline ./...)
  rm -f "$WT/leak_test.go"
  api=fail
  if ( cd "$WT" && go doc -all . ) >"$cell/grade-api.txt" 2>/dev/null; then
    local names
    names=$(api_names | sort -u | tr '\n' ' ')
    # Exactly these names. A extra exported symbol fails the surface diff.
    if [ "$names" = "Cache ErrClosed Loader New " ] || [ "$names" = "$(printf '%s\n' Cache ErrClosed Loader New | sort | tr '\n' ' ')" ]; then
      api=pass
    fi
    # Order from go doc is not sorted. Compare as a set.
    if python3 - "$cell/grade-api.txt" <<'PY'
import re, sys
text = open(sys.argv[1], encoding="utf-8", errors="replace").read()
names = set()
for line in text.splitlines():
    m = re.match(r'^(?:func|type|var|const)\s+([A-Za-z0-9_]+)', line)
    if m and m.group(1)[:1].isupper():
        names.add(m.group(1))
want = {"New", "Cache", "Loader", "ErrClosed"}
sys.exit(0 if names == want else 1)
PY
    then api=pass; else api=fail; fi
  fi
  loc=$(find "$WT" -maxdepth 1 -name '*.go' ! -name '*_test.go' -print0 \
    | xargs -0 wc -l 2>/dev/null \
    | awk 'NF>=2 && $2 != "total" {s+=$1} END{print s+0}')
  # Advisory. Recorded. Never a pass/fail input to objective_quality.
  if [ -f "$STRESSSRC" ]; then
    mkdir -p "$cell/stress"
    cp "$STRESSSRC" "$cell/stress/main.go"
    # The stress binary imports the cell. Run it from a tiny module that
    # replaces blockcache with the worktree. Failure is recorded, not graded.
    if ( cd "$cell/stress" && go mod init stress >/dev/null 2>&1 \
         && go mod edit -replace blockcache="$WT" \
         && GOTOOLCHAIN=local go run -race . ) >"$cell/grade-stress.txt" 2>&1; then
      stress=ran
    else
      stress=recorded-fail
    fi
  fi
  printf 'build=%s vet=%s public=%s race=%s gen=%s gen_race=%s leak=%s api=%s loc=%s stress=%s objective_quality=partial\n' \
    "$build" "$vet" "$public" "$race_public" "$gen" "$gen_race" "$leak" "$api" "$loc" "$stress"
}

grade_walcodec(){
  local build vet v1reg v2pub gen api loc ratio=n/a
  # Phase-2 worktree is the graded tree. v1 tests stay in it (regression).
  # v2 tests are added for the grade if the cell did not leave them.
  cp "$PTSRC" "$WT/walcodec_public_test.go"
  cp "$PT2SRC" "$WT/walcodec_v2_public_test.go"
  build=$(grade_go build go build ./...)
  vet=$(grade_go vet go vet ./...)
  v1reg=$(grade_go v1reg go test -count=1 -run 'TestRoundTripPutDelete|TestCRCFlip|TestUnknownType|TestTruncatedFrame|TestOversizeLen|TestEmptyRecsVersionOnly|TestDecodeNilEmpty|TestDuplicateKeysKeptInOrder|TestEmptyVsNilValue' ./...)
  v2pub=$(grade_go v2 go test -count=1 -run 'TestBatch|TestNested|TestV1|TestMixed|TestTruncatedBatch|TestVersion3' ./...)
  cp "$HELDOUT" "$WT/generalization_test.go"
  gen=$(grade_go gen go test -count=1 -run 'TestHeld' ./...)
  rm -f "$WT/generalization_test.go"
  api=fail
  if ( cd "$WT" && go doc -all . ) >"$cell/grade-api.txt" 2>/dev/null; then
    if python3 - "$cell/grade-api.txt" <<'PY'
import re, sys
text = open(sys.argv[1], encoding="utf-8", errors="replace").read()
names = set()
for line in text.splitlines():
    m = re.match(r'^(?:func|type|var|const)\s+([A-Za-z0-9_]+)', line)
    if m and m.group(1)[:1].isupper():
        names.add(m.group(1))
need = {"Put", "Delete", "Record", "Encode", "Decode", "ErrCorrupt", "Batch"}
# v1 symbols must be present. Additions beyond Batch are allowed.
# A missing v1 symbol, or a rename, fails. Extra names besides Batch are
# additions and are allowed.
sys.exit(0 if need <= names else 1)
PY
    then api=pass; fi
  fi
  loc=$(find "$WT" -maxdepth 1 -name '*.go' ! -name '*_test.go' -print0 \
    | xargs -0 wc -l 2>/dev/null \
    | awk 'NF>=2 && $2 != "total" {s+=$1} END{print s+0}')
  if [ -d "$cell/snapshot-v1" ] && [ -d "$cell/snapshot-v2" ]; then
    ratio=$(cd "$SRC/walcodec/harness" && go run diff.go "$cell/snapshot-v1" "$cell/snapshot-v2" 2>"$cell/grade-diff.err" | awk -F= '/^additive_ratio=/{print $2}')
    ratio=${ratio:-n/a}
  fi
  printf 'build=%s vet=%s v1_regression=%s v2_public=%s gen=%s api=%s loc=%s additive_ratio=%s headline=phase2_convergence+v1_regression+api\n' \
    "$build" "$vet" "$v1reg" "$v2pub" "$gen" "$api" "$loc" "$ratio"
}

grade_cell(){
  case "$PROFILE" in
    resolver) grade_resolver ;;
    blockcache) grade_blockcache ;;
    walcodec) grade_walcodec ;;
  esac
}

record_grade(){
  # The grade string is recorded whole. Per-profile fields stay in the row
  # as a single grade field so a later reader does not have to know which
  # columns a profile emitted. The mechanical fields are also split for A.
  local grade=$1
  case "$PROFILE" in
    walcodec) record_walcodec_row "$grade" ;;
    *) record_row "$grade" ;;
  esac
}

ensure_grok_home
PROFILE_ARG=${1:-}
MODE=${2:-}
case "$PROFILE_ARG" in
  blockcache) use_blockcache ;;
  walcodec) use_walcodec ;;
  *)
    echo "usage: driver.sh blockcache|walcodec calibrate|matrix|all" >&2
    echo "refusing to run with no profile: a bare start would hit the completed resolver series" >&2
    exit 2
    ;;
esac
for f in "$SPECSRC" "$PTSRC" "$HELDOUT"; do
  if [ ! -f "$f" ]; then
    echo "missing instrument file: $f" >&2
    exit 2
  fi
done
if [ "$PROFILE" = walcodec ] && { [ ! -f "$AMENDSRC" ] || [ ! -f "$PT2SRC" ] || [ ! -f "$DIFFSRC" ]; }; then
  echo "missing walcodec phase-2 file" >&2
  exit 2
fi

run_selected(){
  case "$MODE" in
    calibrate)
      run_calibration "$SERIES"
      ;;
    matrix)
      bind_series
      log "==== MATRIX START ($(date)) profile=$PROFILE protocol=isolated-2026-10-30 ceiling=\$$CEILING round_tokens=$ROUND_TOKEN_CAP cell_tokens=$CELL_TOKEN_CAP ===="
      run_matrix
      log "==== MATRIX DONE ($(date)) ===="
      ;;
    all)
      if run_calibration "$SERIES"; then
        bind_series
        log "==== MATRIX START after clean calibration ($(date)) profile=$PROFILE ===="
        run_matrix
        log "==== MATRIX DONE ($(date)) ===="
      else
        log "==== MATRIX NOT STARTED: calibration did_not_finish or session notes rose ===="
        exit 1
      fi
      ;;
    *)
      echo "usage: driver.sh blockcache|walcodec calibrate|matrix|all" >&2
      exit 2
      ;;
  esac
}
run_selected
