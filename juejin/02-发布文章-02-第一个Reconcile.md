# 从零写一个 K8s Operator：kubebuilder 初始化到第一个 Reconcile

> 本系列第一篇[《HPA 只会伸缩不会自愈？我设计了一个 K8s 应用自愈 Operator》](https://juejin.cn)讲清楚了 KubeSentry 的整体设计。本篇开始动手：从 `kubebuilder init` 开始，一步步写出第一个能跑的 Reconcile 循环，并用 envtest 给它写上测试。跟着做完，你就拥有了一个自己的 Operator 骨架。

## 一、准备环境

只需要四样东西：

| 工具 | 版本 | 用途 |
|---|---|---|
| Go | ≥ 1.22 | 项目语言 |
| kubebuilder | v4 | 脚手架（内含 controller-gen） |
| kind | 任意 | 本地 K8s 集群（基于 Docker） |
| kubectl | ≥ 1.28 | 和集群交互 |

```bash
# 初始化项目：--domain 是 CRD 的 API group 后缀，--repo 是 module 路径
kubebuilder init --domain sentry.io --repo github.com/weirong-ui/kubesentry

# 创建 API：同时生成 CRD（--resource）和控制器（--controller）
kubebuilder create api --group apps --version v1alpha1 --kind App --resource --controller
```

回答 `Create Resource [y/n]` 和 `Create Controller [y/n]` 都填 `y`，`Create Controller(s)` 之后它会帮你跑 `make manifests generate`。

生成的骨架长这样：

```
kubesentry/
├── api/v1alpha1/
│   ├── groupversion_info.go   # GVK 注册（Group=apps.sentry.io Version=v1alpha1）
│   ├── app_types.go           # CRD 的 Spec/Status 定义
│   └── zz_generated.deepcopy.go  # controller-gen 生成的深拷贝（不要手改）
├── internal/controller/
│   └── app_controller.go      # Reconciler
├── cmd/main.go                # Manager 入口
├── config/                    # CRD/RBAC/Webhook/Manager 的 kustomize 配置
├── Dockerfile
└── Makefile
```

## 二、先搞懂 Manager 在干什么

`cmd/main.go` 的核心就一段：

```go
mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
    Scheme:                 scheme,
    HealthProbeBindAddress: probeAddr,
    LeaderElection:         enableLeaderElection,
    LeaderElectionID:       "kubesentry.sentry.io",
})

if err := (&controller.AppReconciler{
    Client: mgr.GetClient(),
    Scheme: mgr.GetScheme(),
}).SetupWithManager(mgr); err != nil {
    // ...
}
mgr.Start(ctrl.SetupSignalHandler())
```

Manager 是"大管家"，它替你管了四件事：

1. **Scheme**：GVK ↔ Go 类型的注册表。`appsv1alpha1.AddToScheme(scheme)` 之后，Client 才知道 `App` 该发给哪个 API group。
2. **Cache**：本地 Informer 缓存。Reconciler 里的 `r.Get()` 读的是缓存而不是每次都打 apiserver——这就是 Informer 机制的第一层体现，后面 P4 我们手写它。
3. **Client**：读走缓存、写直连 apiserver 的封装客户端。
4. **LeaderElection**：多副本部署时保证只有一个实例在干活（自愈系统自己绝不能脑裂）。

## 三、定义 App CRD

`app_types.go` 是整个项目最重要的一张"脸"。核心片段：

```go
// +kubebuilder:object:generate=true
// +groupName=apps.sentry.io
package v1alpha1

// +kubebuilder:validation:Enum=QPS;P99Latency;ErrorRate
type MetricType string

type MetricTarget struct {
    Type   MetricType `json:"type"`
    Target string     `json:"target"` // "1000" / "200ms" / "0.01"
}

type ScalingPolicy struct {
    // +kubebuilder:validation:Minimum=1
    MinReplicas int32 `json:"minReplicas"`
    // +kubebuilder:validation:Minimum=0
    // +kubebuilder:default=120
    CooldownSeconds int            `json:"cooldownSeconds"`
    Metrics         []MetricTarget `json:"metrics"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
type App struct {
    metav1.TypeMeta   `json:",inline"`
    metav1.ObjectMeta `json:"metadata,omitempty"`
    Spec   AppSpec   `json:"spec,omitempty"`
    Status AppStatus `json:"status,omitempty"`
}
```

几个 marker 值得专门说：

- `+kubebuilder:validation:*`：生成的 CRD 会带 OpenAPI 校验，`kubectl apply` 阶段就拦住非法值，**不需要等 Controller 报错**。
- `+kubebuilder:subresource:status`：开启 `/status` 子资源。这样写 Status 不会动 Spec，也避免 Spec 写权限扩散。
- `+kubebuilder:printcolumn`：让 `kubectl get app` 能显示 Phase、Replicas 列，用户体验直接上一个档次。

改完执行 `make manifests generate`，controller-gen 会生成两样东西：`config/crd/bases/` 下的 CRD YAML 和 `zz_generated.deepcopy.go`。**DeepCopy 必须生成**——K8s 的对象在缓存里是共享的，任何修改前都要 `DeepCopy()`。

## 四、第一个 Reconcile：把 App 变成 Deployment

Reconciler 的全部工作只有一个函数：`Reconcile(ctx, req)`。KubeSentry P0 版的核心逻辑：

```go
func (r *AppReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
    var app appsv1alpha1.App
    if err := r.Get(ctx, req.NamespacedName, &app); err != nil {
        if apierrors.IsNotFound(err) {
            return ctrl.Result{}, nil // 对象已删除，直接退出
        }
        return ctrl.Result{}, err
    }

    // 1. Finalizer：删除前先做清理
    if !app.DeletionTimestamp.IsZero() {
        if controllerutil.ContainsFinalizer(&app, appFinalizer) {
            controllerutil.RemoveFinalizer(&app, appFinalizer)
            return ctrl.Result{}, r.Update(ctx, &app)
        }
        return ctrl.Result{}, nil
    }
    if !controllerutil.ContainsFinalizer(&app, appFinalizer) {
        controllerutil.AddFinalizer(&app, appFinalizer)
        if err := r.Update(ctx, &app); err != nil {
            return ctrl.Result{}, err
        }
        return ctrl.Result{RequeueAfter: time.Second}, nil
    }

    // 2. 幂等地确保 Deployment 与 Spec 一致
    if _, err := r.ensureDeployment(ctx, &app); err != nil {
        return ctrl.Result{}, err
    }

    // 3. 写回状态
    patch := client.MergeFrom(app.DeepCopy())
    app.Status.Phase = appsv1alpha1.PhaseStable
    if err := r.Status().Patch(ctx, &app, patch); err != nil {
        return ctrl.Result{}, err
    }
    return ctrl.Result{RequeueAfter: 15 * time.Second}, nil
}
```

`ensureDeployment` 用的是 `controllerutil.CreateOrUpdate`：

```go
want := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: app.Name, Namespace: app.Namespace}}
_, err := controllerutil.CreateOrUpdate(ctx, r.Client, want, func() error {
    want.Spec.Replicas = &replicas
    want.Spec.Selector = &metav1.LabelSelector{MatchLabels: managedLabels}
    want.Spec.Template.Spec.Containers = []corev1.Container{{Name: app.Name, Image: app.Spec.Image}}
    return controllerutil.SetControllerReference(app, want, r.Scheme)
})
```

### 这里藏着 4 个新手必踩的坑

**坑 1：Reconcile 不是事件处理器。** 它不接收"事件"，只接收"这个对象需要调和"的通知。你不知道触发原因（是 Spec 改了？Deployment 被人删了？还是上一轮失败重试？）。所以唯一正确的写法是：**每次都从头读最新状态，全量对齐到期望状态**。这叫 level-triggered，K8s 控制器的灵魂。

**坑 2：直接 Update 缓存里的对象。** `r.Get()` 返回的对象来自共享缓存，直接改它再 Update 会污染缓存并产生并发写。必须先 `DeepCopy()`。本文代码里 `CreateOrUpdate` 内部处理了这一点，但手写更新逻辑时极易踩雷。

**坑 3：Status 更新又触发自己 → 无限 Reconcile。** 如果把 Status 写进 Spec 更新（没用 `/status` 子资源），或者 Status 内容每次都在变（比如写了个时间戳），就会自己触发自己，死循环到 QPS 爆炸。对策：Status 永远走 `r.Status().Patch()`，且内容只有在变化时才写。

**坑 4：忘了 Finalizer。** 没有它，App 删了对应的 Deployment 会被 OwnerReference 级联删掉——看起来没问题，但未来 KubeSentry 要在删除前导出自愈审计记录，就必须有 Finalizer 挡一道。**Finalizer 里忘了 `RemoveFinalizer` 的话对象永远删不掉**，这是 Operator 生产事故 Top3。

### Owns 与 OwnerReference

```go
func (r *AppReconciler) SetupWithManager(mgr ctrl.Manager) error {
    return ctrl.NewControllerManagedBy(mgr).
        For(&appsv1alpha1.App{}).
        Owns(&appsv1.Deployment{}).
        Complete(r)
}
```

`SetControllerReference` 建立了 OwnerReference（同时让 Deployment 随 App 级联删除），`Owns(&Deployment{})` 则让**受管 Deployment 的任何变化都触发本控制器的 Reconcile**——有人手动把副本数改成 100？下一秒就会被对齐回来。这就是"声明式"的力量。

## 五、用 envtest 写第一个测试

Operator 的测试不用起真实集群。envtest 会拉起真实的 apiserver 和 etcd（不含 kubelet/controller-manager）：

```go
func TestAppReconcile(t *testing.T) {
    testEnv := &envtest.Environment{
        CRDDirectoryPaths: []string{filepath.Join("..", "..", "config", "crd", "bases")},
    }
    cfg, _ := testEnv.Start()
    defer testEnv.Stop()

    k8sClient, _ := client.New(cfg, client.Options{Scheme: scheme})
    reconciler := &AppReconciler{Client: k8sClient, Scheme: scheme}

    // 创建 App → 执行 Reconcile → 断言 Deployment 被创建
    app := newTestApp("order-service", 3)
    _ = k8sClient.Create(ctx, app)
    _, err := reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(app)})
    assert.NoError(t, err)

    var dep appsv1.Deployment
    _ = k8sClient.Get(ctx, types.NamespacedName{Name: "order-service", Namespace: "default"}, &dep)
    assert.Equal(t, int32(3), *dep.Spec.Replicas)
}
```

Reconciler 的依赖只有 `client.Client` 接口——这就是把**决策逻辑和执行逻辑分离**（我们 scaler 包的设计）能换来高可测性的原因。跑 `make test`，绿了。

## 六、跑起来看效果

```bash
kind create cluster
make install                      # 装 CRD
make run                          # 本地跑 Operator（连 kind）
# 另开终端：
kubectl apply -f config/samples/app_v1alpha1_app.yaml
kubectl get deploy -w
# NAME    READY   UP-TO-DATE   AVAILABLE
# sample  2/2     2            2          ← KubeSentry 创建的

# 手动改副本数，看它被"掰"回来：
kubectl scale deploy sample --replicas=10
# 一瞬间又回到 2 —— Owns + level-triggered 在工作
```

到这里，P0 里程碑完成：一个有 CRD、有幂等 Reconcile、有 Finalizer、有测试的 Operator 骨架。代码都在 [github.com/weirong-ui/kubesentry](https://github.com/weirong-ui/kubesentry)（P0 标签）。

## 七、下一篇预告

现在它只会"照着 Spec 抄 Deployment"，还不会思考。下一篇我们让它开始思考：《为什么 HPA 会震荡？手写一个带冷却窗口的伸缩决策引擎》——把 QPS/P99 指标接进来，实现多信号决策 + 防震荡算法。

觉得有用的话点个赞，仓库点个 star 追更不迷路。踩坑问题欢迎评论区拍砖。

---

系列目录：01 项目设计 ｜ **02 第一个 Reconcile（本篇）** ｜ 03 伸缩决策引擎 ｜ 04 分级自愈 ｜ 05 Admission Webhook ｜ 06 手写 mini-Informer
