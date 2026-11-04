#!/usr/bin/env bash
# 压测编排：同一流量时间线注入到 HPA 组与 KubeStasis 组，5 秒粒度采样。
#
# 时间线（秒）：
#   0-120    基线流量（每侧 12 QPS 稳态）
#   120-360  突发 #1（每侧 +150 QPS = 162 QPS，单 Pod 容量约 62 QPS → 必须扩容）
#   360-480  空闲 #1
#   480-600  突发 #2
#   600-1140 空闲 #2：观察缩容行为（HPA 默认 300s 稳定窗口 vs KubeStasis 冷却+渐进）
#
# 用法：hack/benchmark.sh [输出CSV] [总时长秒]
# 注意：不用 set -e —— 采集循环里单次 kubectl 失败（metrics 瞬时缺数、
# port-forward 抖动）不该中断整轮 20 分钟的压测，缺值按 0 记。
set -uo pipefail

NS=bench
OUT=${1:-docs/benchmark/data.csv}
DURATION=${2:-1140}

mkdir -p "$(dirname "$OUT")"

log() { printf '[%s] %s\n' "$(date +%H:%M:%S)" "$*"; }

scale_burst() {
  local n=$1
  kubectl -n "$NS" scale deploy/loadgen-burst-hpa --replicas="$n" >/dev/null
  kubectl -n "$NS" scale deploy/loadgen-burst-ks --replicas="$n" >/dev/null
  log "burst loadgens -> $n"
}

# 单指标读数，缺省 0（HPA 刚创建时 status 是空的）
val_or_zero() { local v; v=$("$@" 2>/dev/null || true); echo "${v:-0}"; }

hpa_cur()  { val_or_zero kubectl get hpa bench-hpa -n "$NS" -o jsonpath='{.status.currentReplicas}'; }
hpa_des()  { val_or_zero kubectl get hpa bench-hpa -n "$NS" -o jsonpath='{.status.desiredReplicas}'; }
ks_spec()  { val_or_zero kubectl get app bench-ks -n "$NS" -o jsonpath='{.spec.replicas}'; }
ks_ready() { val_or_zero kubectl get app bench-ks -n "$NS" -o jsonpath='{.status.currentReplicas}'; }

# CPU 毫核：按 Pod 名前缀分组求和（metrics-server 数据）
cpu_m() {
  kubectl top pods -n "$NS" --no-headers 2>/dev/null \
    | awk -v p="$1" '$1 ~ "^"p {gsub("m","",$2); s+=$2} END {printf "%d", s+0}'
}

# Prometheus 侧 QPS（每侧实际服务速率）。port-forward 一次，循环复用。
PF_PORT=19090
if ! curl -s --max-time 2 "http://127.0.0.1:${PF_PORT}/-/ready" >/dev/null 2>&1; then
  kubectl -n "$NS" port-forward svc/prometheus ${PF_PORT}:9090 >/dev/null 2>&1 &
  PF_PID=$!
  trap 'kill ${PF_PID:-0} 2>/dev/null || true' EXIT
  for _ in $(seq 1 20); do
    curl -s --max-time 1 "http://127.0.0.1:${PF_PORT}/-/ready" >/dev/null 2>&1 && break
    sleep 1
  done
fi

qps_of() {
  local app=$1
  curl -s -G --max-time 3 "http://127.0.0.1:${PF_PORT}/api/v1/query" \
    --data-urlencode "query=sum(rate(http_requests_total{app=\"${app}\",namespace=\"${NS}\"}[1m]))" 2>/dev/null \
    | python -c "import json,sys
d=json.load(sys.stdin)
r=d['data']['result']
print('%.1f' % float(r[0]['value'][1]) if r else '0')" 2>/dev/null || echo 0
}

echo "ts,phase,hpa_desired,hpa_current,ks_spec,ks_ready,hpa_cpu_m,ks_cpu_m,hpa_qps,ks_qps" > "$OUT"
log "benchmark start: ${DURATION}s -> $OUT"

# 时间轴用**挂钟**（SECONDS）驱动，不用循环计数器：一轮采样要 5-7 秒，
# 计数器会把 20 分钟的压测拉成 35 分钟，而 HPA 的 scaleDown stabilization
# 是真实时间（300s）——被拉伸的时间轴会直接毁掉对比结论。
START=$SECONDS
fired=" "
while :; do
  t=$((SECONDS - START))
  (( t > DURATION )) && break

  # 到点触发一次负载切换（t 可能跨过边界，所以用"已触发"集合去重）
  for ev in 120 360 480 600; do
    [[ " $fired " == *" $ev "* ]] && continue
    if (( t >= ev )); then
      case $ev in
        120 | 480) scale_burst 1 ;;
        360 | 600) scale_burst 0 ;;
      esac
      fired="$fired $ev"
    fi
  done

  if   (( t < 120 )); then label=baseline
  elif (( t < 360 )); then label=burst1
  elif (( t < 480 )); then label=idle1
  elif (( t < 600 )); then label=burst2
  else                     label=idle2
  fi

  printf '%d,%s,%s,%s,%s,%s,%s,%s,%s,%s\n' \
    "$t" "$label" "$(hpa_des)" "$(hpa_cur)" "$(ks_spec)" "$(ks_ready)" \
    "$(cpu_m bench-hpa)" "$(cpu_m bench-ks)" "$(qps_of bench-hpa)" "$(qps_of bench-ks)" >> "$OUT"

  if (( t % 60 < 5 )); then
    log "t=${t}s phase=${label} hpa=$(hpa_cur) ks=$(ks_spec) qps_hpa=$(qps_of bench-hpa) qps_ks=$(qps_of bench-ks)"
  fi

  # 对齐到下一个 5 秒边界（采样本身耗时已超过就立即进下一轮）
  next=$(( (t / 5 + 1) * 5 ))
  while (( SECONDS - START < next )); do sleep 0.3; done
done

log "benchmark finished. loadgen 失败计数："
for lg in loadgen-steady-hpa loadgen-burst-hpa loadgen-steady-ks loadgen-burst-ks; do
  printf '  %-20s %s\n' "$lg" "$(kubectl -n "$NS" logs deploy/$lg --tail=200 2>/dev/null | grep 'loadgen stats' | tail -1)"
done
