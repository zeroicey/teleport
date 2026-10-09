# 公开文档里写"密钥存在服务器哪个文件"：等于给了一份精确的取宝地图

**Severity:** 🟠 medium · **First hit:** 2026-10-10 · **Hits since:** 0

## Symptom

无报错、无告警，页面正常渲染，看起来一切正常。触发点是我自己写的测试：

```
--- FAIL: TestGuideContainsNoSecret (0.00s)
    aidoc_test.go:120: guide mentions "AGENT_SECRET_KEY=" — the guide must not carry secret material
```

## Root cause

`/ai.md` 与 `/ai` 是**公开、免鉴权**的（这是刻意设计：agent 必须在拿到密钥之前就能读到
"你需要一个密钥"）。初版第 9 节"给人类的附录"出于好心写了运维便利信息：

```
Agent 密钥的位置（部署在服务器上）：/data/services/teleport/teleport.env
里的 `AGENT_SECRET_KEY=...`，取出来发给 AI 即可。
```

**这不是密钥泄漏，但是泄漏的一半** —— 它精确指出了密钥在哪个主机的哪个文件、哪个变量名。
任何能读到这个页面的人（包括搜索引擎爬虫、以及任何被用户随手转发的第三方）都获得了一份
「去哪个文件取」的完整指引；配合其它任意一个弱点（弱 SSH 口令、路径遍历、备份文件暴露）
就直接串成了完整攻击链。

**为什么会写出来**：写文档时的心智是"读者是我自己/同事"，忘了这个页面的受众是
**互联网上的任何人**。公开文档的威胁模型与内部 wiki 完全不同。

**为什么容易漏**：这段文字看起来是「运维提示」而不是「敏感信息」。人对"密钥"有警觉，
对"密钥放在哪"没有 —— 但后者正是攻击者最缺的那一环。

## Fix

改为声明边界，而不是提供路径：

```markdown
- 本文档是**公开可读**的，因此这里不会写任何密钥、也不会写服务器上密钥存在哪里。
  需要 agent 密钥时，请运维方从服务端配置中取出后私下转交。
```

（`backend/internal/aidoc/guide.md` 第 9 节）

## Guard

`backend/internal/aidoc/aidoc_test.go` 的 `TestGuideContainsNoSecret`：扫描渲染后的文档，
禁止出现 `AGENT_SECRET_KEY=`（带赋值号，即后面跟了值）、`SESSION_SECRET`、
`ADMIN_PASSWORD_HASH`。同时**要求**出现不带赋值的 `AGENT_SECRET_KEY` —— 文档必须告诉
agent 去索取哪个凭据，只是不许给出值或地址。

`TestAIGuideDoesNotLeakSecrets` 在 HTTP 层再兜一层：直接比对三个真实凭据串不出现在响应里。

> **推广**：任何**公开**页面/文档都要过一遍"这条信息帮不帮攻击者定位目标"。
> "配置在哪"、"内网地址"、"内部主机名"、"端口拓扑"都属于此类 —— 它们单独不致命，
> 但把攻击成本从"盲扫"降到"照做"。私有文档可以写，公开文档不行。
