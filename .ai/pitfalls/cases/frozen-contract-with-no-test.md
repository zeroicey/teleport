# 最危险的那条契约没有任何测试守住

**发现方式**：为重写做 P0 清点时，逐条 grep「14 项冻结契约各自被哪个测试钉住」。
结果是 21 项检查里 **3 项完全没有测试引用**，而且正好是风险最高的那 3 项：

```
1. agentkey 派生标签 (derive)   *** NO TEST ***
1. agentkey 派生标签 (token)    *** NO TEST ***
1. agentkey 续期标签            *** NO TEST ***
```

**为什么这比"少一个测试"严重得多**：agent token 是**派生**的，服务端只存 `sha256(token)`，
无法反查、只能重算：

```
token    = base64url(HMAC-SHA256(K_derive, "teleport/agent-key/v1" | appID | claimSecret))
K_derive = HMAC-SHA256(SESSION_SECRET, "teleport/derive/agent-key/v1")
```

所以只要那两个标签字符串（或字节顺序）变了，**所有已发出的 agent 密钥立刻无法认证** ——
而代码照样编译、其余 243 个测试照样全绿。故障只会表现为「一群没人盯着的 agent 突然开始 401」。

也就是说：**一次看起来无害的重写（"把字符串统一一下"）可以静默地吊销整个用户群的凭据。**

## 为什么原有测试没覆盖

`internal/agentkey/agentkey_test.go` 的 7 个测试全是**性质测试**：
确定性、域分离、URL 安全、熵、哈希是 hex sha256、prefix 稳定、ID 是 UUIDv4。
它们验证的是"派生是良构的"，**没有一条断言"派生出来的具体字节是什么"**。

`TestDeriveTokenIsDeterministicAndDomainSeparated` 只比较「同输入 → 同输出」「不同输入 → 不同输出」，
这在整个标签被替换掉之后**依然全部成立**。而且测试里没有任何硬编码的长 token 字面量
（唯一的长字符串是 base64url 的字母表），所以也没有黄金值可以隐式兜住它。

## 修复

新增 4 个测试（`247` 个测试函数），两种互补的钉法：

1. **标签字面量**：`derivationLabel != "teleport/derive/agent-key/v1"` 直接失败。
   断言右侧是**字面量**而不是同一个常量 —— 否则测试就是同义反复。
2. **黄金字节**：固定输入下的 `KDerive` hex、`DeriveToken`、`Prefix`、`HashToken` 精确值。
   这条能抓到字面量检查抓不到的东西：**输入顺序变了、摘要换了、base64 字母表换了**
   —— 那些改动依然满足文档里的公式，但产生的 token 不同。
3. **独立重算**：用 `crypto/hmac` 按文档公式重写一遍，与实现不共享任何代码。
   它守的是"实现与注释里的公式不许分家"。
4. **续期标签**（在 `internal/api`，因为常量在那里）：同样三条 + 一条域分离断言
   （续期 secret 与 token 在相同 key id 下不得碰撞）。

## 守卫的守卫（反证）

按本项目的规矩，新守卫必须证明它在被守的东西变了时会红：

| 反证 | 结果 |
| --- | --- |
| 把 `teleport/derive/agent-key/v1` 改成 `relay/derive/agent-key/v1` | 3 个测试变红（标签 + 黄金值 + 独立重算） |
| 只交换 `DeriveToken` 里两个 `mac.Write` 的顺序（标签不变） | `TestDerivedValuesAreFrozen` 变红 |
| 还原 | 文件 sha256 与改动前**逐字节一致** |

## 教训

**「契约」与「被测试钉住的契约」是两件事。** 一个字符串可以出现在代码里、出现在注释里、
出现在文档里、被所有人当成不可改的约定 —— 同时没有任何东西在它被改时发出声音。

清点方式很便宜：对每一条冻结项，grep 它是否被 `*_test.go` 引用。
**零引用 + 高风险 = 立刻补黄金值测试**，而且黄金值必须来自**当前实现**（先算出来再写进测试），
不能凭记忆写 —— 凭记忆写的黄金值只会把 bug 固化成期望。
