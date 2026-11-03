# Demo #2：分级自愈 + 金丝雀自动回滚

> 验证时间：2026-11-03 ｜ 环境：Docker Desktop Kubernetes v1.36.1
> operator 参数：`--prometheus-url=http://prometheus.demo.svc:9090 --prometheus-window=1m --heal-cooldown=30s`（演示用小冷却，生产默认 3m）

## 场景 A：发布一个 CrashLoop 版本 → L2 摘除 → 升级 L3 回滚 → 自愈

```bash
# 1. 健康版本先跑稳（healdemo = kubestasis-demoapp:v1, 2 副本, health.strategy=graduated）
# 2. 发布坏版本（同一二进制的 -crash 模式，启动即退出）
kubectl patch app healdemo --type=merge -p '{"spec":{"image":"kubestasis-demoapp:crash"}}'
```

实录（事件按时间排序）：

```
T+0     发布 crash 镜像，两个 Pod 进入 Error/CrashLoopBackOff
T+150s  KubeStasisHealAction  L2 Isolate: deleted 1 unhealthy pod(s): [healdemo-798bd556d-n9w9h]
        # 删除后重建的 Pod 依旧 CrashLoop —— L2 治不好"版本本身坏了"
T+171s  KubeStasisHealAction  L3 Rollback: image kubestasis-demoapp:crash -> kubestasis-demoapp:v1 (revision 1)
        # 冷却期内故障复发 → 自动升级到 L3，从 ReplicaSet 修订历史找到上一版本镜像
T+173s  KubeStasisReconcile   app available: 2/2 replicas ready
        # 新 Pod 跑回 v1，restarts=0，phase=Stable
```

终态 status（节选）：

```json
{
  "phase": "Stable",
  "currentImage": "kubestasis-demoapp:v1",
  "lastHealLevel": 2,
  "conditions": [{"type": "Available", "status": "True", "message": "2/2 replicas ready"}]
}
```

**设计要点**：回滚改的是 `App.Spec.Image` 而不是直接改 Deployment —— 回到 CR 层面
让 Reconcile 自然收敛，回滚和正常发布走同一条路径，审计上也能看到"是 KubeStasis
改的 spec"。

## 场景 B：金丝雀窗口内错误率超阈值 → 自动回滚

```bash
# canarydemo: health.canary.enabled=true, observationSeconds=180, errorRateThreshold=0.05
# 持续打流 200 QPS（canaryload），先确认错误率 0.0
kubectl patch app canarydemo --type=merge -p '{"spec":{"image":"kubestasis-demoapp:v2-err"}}'  # 50% 错误率的版本
```

实录：

```
T+0    发布 v2-err（50% 请求返回 500）
T+~20s KubeStasisRollback  canary failed (error rate exceeded 0.05): rolled back to revision 1
       # status.rollbackRevision=1，spec.image 自动回到 v1，窗口关闭（不会因回滚重开窗口）
```

## 已知的护栏行为（刻意为之）

- 已到 maxReplicas 时 L1 不再扩（事件：`no action available (observing or at limit)`）
- 已回滚过仍在坏 → 冷却期内不再动作（防止原地空转）
- 无更早版本可退 → 只告警 `KubeStasisCanaryFailed`，不空转
- 错误率查不到数据 → 金丝雀视为通过（宁可放过，绝不误回滚）
- `strategy: observability-only` → 只发 Warning 事件，不动集群

## 已知噪音（不阻塞）

乐观锁冲突（`the object has been modified`）在伸缩/自愈并发写同一对象时偶发，
Reconcile 退避重试后自动恢复。事件里会留一条 `KubeStasisError`，观感差但无害。
优化方向记录在 docs/STATUS.md「已知噪音」。
