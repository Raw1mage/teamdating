# teamdating — 架構

多人線上旅行日期共訂系統。提案 → 可選項 → 複選表態 → 即時同步。

**唯一硬約束：零第三方套件**（使用者裁示）。Go 標準庫後端 + 零依賴 ES module
前端。`go.mod` 的 require 區塊必須為空，由 `scripts/check-zero-deps.sh` 與
`zerodeps_test.go` 兩層把關。

> 本檔是**全貌同步**文件，不是變更流水帳。與程式現況不符時以程式為準並立即改寫。
> 決策的完整理由住在 `plans/app_travel-date-vote/design.md`（DD-1 … DD-21）；
> 本檔只記「系統長什麼樣、邊界在哪、訊號在哪」。

---

## 1. 模組邊界

```
cmd/teamdating/main.go     旗標解析、兩個 listener 的組裝、生命週期
  └─ assets.go             module root 的 //go:embed web（刻意不放 cmd 下）
internal/store/            權威狀態 + write-ahead 事件日誌（唯一可變狀態）
internal/auth/             純函式授權（無 I/O、無時鐘、無變更）
internal/httpapi/          HTTP 邊界、SSE、限流、壓實觸發、撞名分流、magic-link 認證
internal/mailer/           magic-link 認證信寄送（net/smtp 手動 StartTLS，零第三方）
internal/hub/              SSE 扇出、presence、cursor
web/                       零依賴 ES module 前端
```

**依賴方向是單向的**：`httpapi → auth`、`httpapi → store`、`httpapi → hub`。
`store` 與 `auth` 互不知道對方；`auth` 透過一個三方法唯讀 interface 看 store。

### 為什麼 `auth.State` 是 interface 而不是 `*store.Store`

兩個理由都承重：

1. **無副作用是結構性的，不是註解裡的承諾**——那個 interface 沒有任何可寫方法，
   一條想要變更狀態的規則**編譯不過**。
2. **拒絕案例與允許案例一樣便宜**——拒絕案例昂貴時它們會安靜地不被寫，而一個
   `return Allow` 的實作會通過整套測試。

方法刻意窄：每個只回一個純量加一個存在旗標，型別系統無法表達「把整個提案讀出來
自己判斷」。存在旗標讓「沒有這個提案」與「擁有者是空字串」分得開。

---

## 2. 資料流與狀態機

### 2.1 寫入路徑（承重契約，動它要先問）

```
handler → auth.Authorize → store.commit
                              ├─ validate()          持 writeMu
                              ├─ append 事件 + fsync  ← 先落盤
                              ├─ [crashHook]         ← 只有測試會裝
                              ├─ apply()             持 mu.Lock
                              ├─ seq++
                              └─ notify()            ★ 仍在 writeMu 內（DD-21）
```

**順序是設計裁示不是實作細節**（DD-14）：反過來寫也「能動」，在乾淨關機下**完全
無法區分**，只有在 `kill -9` 時才表現成「使用者看到成功但重啟後記錄消失」。

`notify()` 必須在 `writeMu` **內**（DD-21）：放外面會讓兩個並發變更 A→B 進 store
卻 B→A 發通知，對 diff 渲染的客戶端造成靜默分歧。實測轉紅訊號是 `DATA RACE`。

`writeMu` 與 `mu` 不是冗餘：`mu` 讓單次 map 存取安全，但 check-then-act 需要兩步
對其他寫者是原子的；而持 `mu` 跨越 fsync 會讓每個讀者等一次磁碟同步。

### 2.2 壓實（6.3）

計數驅動而非計時器驅動——閒置部署上計時器會永遠重寫同一份快照，燒 fsync 又反覆
取用崩潰窗口。門檻由 `compactAfterWrite` 中介層在寫入請求**完成之後**觸發（不能
在 `commit()` 內，`writeMu` 不可重入）。

兩個 rename 的順序即全部契約：**snapshot 先落地，log 才截斷**。顛倒過來崩在中間
會丟掉全部資料，而兩種順序在乾淨關機下輸出相同。

### 2.3 讀取路徑

所有 accessor 回**拷貝**，套件外永遠拿不到活的 map 參照。聚合量（票數、三態計數、
分數）**當場在鎖內算，從不落成可寫欄位**——落地的計數是可偽造的計數。

### 2.4 事件型別（replay 的封閉集合）

`user_create` / `user_rename` / **`user_add_token`** / **`user_revoke_token`** /
**`user_touch_token`** / `proposal_create` / `proposal_update` /
`proposal_delete` / `proposal_decide` / `proposal_repid` / `option_create` /
`option_update` / `option_delete` / `vote_set` / `vote_delete` / **`user_set_email`** /
**`user_merge`**

replayer 對**不在集合內**的型別報錯並指出行號。靜默跳過會讓「我們掉了一筆記錄」
與「檔案本來就沒有」產生同一個狀態，缺陷因此不可偵測。

**這條性質同時使本改動不可回滾**：一旦寫入任何 `user_*_token` 或 `user_set_email`
事件，舊 binary 讀到就啟動失敗。這是刻意的代價——換來的是「掉一筆」永遠不會偽裝成
「本來就沒有」。部署前備份 `data/` 是唯一的退路。

### 2.5 身分模型：一 user 多 token

```
User.Tokens []TokenRef{Hash, AddedAt, LastSeenAt}   上限 5，滿了淘汰 LastSeenAt 最舊者
byToken     map[hash]uid                            一對多：多把 hash 指向同一個 uid
```

一個人有手機、平板、電腦，每台一把獨立的鑰匙。**`[]TokenRef` 而非「主 token +
備用清單」**：後者讓同一件事有兩個儲存位置，而**只讀主欄位的程式碼會通過所有測試**
（第一把 token 永遠在主欄位裡）。單一 slice 讓「漏看一半」在型別上無法表達。

**LRU 依 `LastSeenAt` 不是 `AddedAt`**。兩者在「照加入順序使用」的情境下答案相同，
而那正是所有天真測試會做的事——所以選錯欄位的淘汰數量完全正確、只是踢錯裝置。
唯一分得出來的是 TV-6（`TestEvictionOrdersByLastSeenNotAdded`）。

`LastSeenAt` 的更新**必須進日誌**（`user_touch_token`），否則重啟後它退回等於
`AddedAt`，LRU 靜默退化成 FIFO。但每個請求寫一筆會讓讀路徑變成 fsync 路徑，故
**節流**：只有距上次 touch 超過 1 小時才寫。代價是 LRU 解析度為小時級——對「踢掉
幾個月沒用的舊手機」這個唯一目的完全足夠，且這個誠實的近似寫在 `TouchTokenIfStale`
的註解裡。

**淘汰決定由事件攜帶（`EvictedHash`），replay 不重算**。重算讀的是被節流的近似值，
可能挑出與當初不同的一把，於是 replay 後的狀態與當初服務的狀態靜默分歧。
**事件記錄決定，不記錄輸入。** 副產品：一行 grep 就能回答「這把 token 是被誰擠掉的」。

**`UserCreateData` 的形狀凍結**（單值 `TokenHash`），轉換在 `applyEvent` 的
`EvUserCreate` 分支。改成 slice 的話，舊日誌的 `{"TokenHash":"h"}` 用新 struct 解碼
**不會回錯**（缺欄位取零值）→ replay 回報成功 → 使用者全部登出 → 日誌零錯誤。
這是本改動最危險的單點，由 `TestUserCreateDataShapeIsFrozen` 讀原始碼釘住——
行為測試對它無能為力，因為根本沒有可觀察的失敗。

snapshot v1 → v2 走**分離的解碼函式**（`loadSnapshotV1` / `loadSnapshotV2Only`），
不是同一個 struct 靠 `omitempty` 區分。同一個 struct 會讓「v1 檔忘了轉換」與
「v2 檔本來就沒有 token」產生**同一個記憶體狀態**，而前者是災難、後者是正常。

**碰名接回**（`handleRegister` 第二分支）：無 cookie ＋名字已在 roster ⇒ 為既有 uid
新增一把 token。這放大了名字碰撞風險（從「打錯一次」變成「在別人身分上留下持久
鑰匙」），是產品擁有者已接受的既有取捨（`read.go:31-44`：便利優先於隔離、無敏感
資料），**不是待修缺陷**。

### 2.6 email 認證（W3C magic-link）與撞名三路分流

**email 最小化（DD-6）**：一般被邀選行程者只需姓名、零摩擦；只有**發起提案者**
在建立提案時才被強制補 email（`handleCreateProposal` commit 前檢查發起者 `Email`
已設，未設回 `EMAIL_REQUIRED_FOR_PROPOSAL` 導向齒輪 modal，非註冊時擋）。

**撞名三路分流**（`handleRegister`，同名 `UsersByName(name)` 非空時）——三條分支
**都先 `countProbe` 計數於分岔之前**，否則 delta 本身變成「email 在不在檔」的側通道
oracle：

```
(a) 帶 email 且命中同名身分  → needEmailVerify  回「請點認證信」，此處不綁定
(b) 帶 email 但無同名符合    → nameCollision    真·同名不同人,選改名 or 後綴 -N
(c) 無 email 但同名帳號有 email → emailRequired  強制補 email 認證
```

`countProbe`（`handlers.go:79`）：`candidateProbes.Add(1)` + operator log（不記提交
名字）。所有「回應匿名 caller 但不改 store」的分支都須經此——**匿名讀路徑刻意不寫
event log**（append 會被濫用 + 舊 binary 無法解析新事件型別），計數是唯一留痕。

**magic-link 認證資料流**（觸發 → 生成 → 寄送 → confirm → 綁定）：

```
POST /api/verify-email {name,email}   ← 無 uid 欄位（DD-8 承重契約）
  → resolveVerifyTarget 兩路：① UserByNameAndEmail 精確命中=merge
                              ② UsersByNameWithoutEmail 恰一無 email 同名=DD-9 claim
  → 查無 → uniform response 且不寄信（防 oracle）
  → newVerifyToken: crypto/rand 256bit → 只存 SHA-256 hash（明文永不落地）
  → ★ 寄信先於落表（TV-10）: mailer.Send 失敗即 SMTP_SEND_FAILED、不建 pending
  → pending.put: {uid,email,name,claim,expiresAt=now+15m,used}  記憶體,不進 events.jsonl
GET /api/verify-email/confirm?t=<token>   ← mail client 只能 GET
  → pending.consume: 單一鎖內 check-and-mark（三態 invalid/expired/used/ok）
  → AddToken(uid, 本裝置 hash) + 若 email 未設則 SetEmail（DD-9 claim 補寫,關掉重複認領洞）
```

**uid 只存 pending 記憶體、永不上線**（DD-8）：verify 端點從不從 body 讀 uid,
一律 server-side 由 name+email 解析目標身分。one-time 用 `used` flag 非 delete
（replay 過期項與已用項要分得開）。

**DD-9 冒領缺口（知情保留）**：舊帳號無 email 在密碼學上無主,單純版允許 email 認證
的同名者認領。使用者裁示先做單純版,冒領缺口記為日後規模化思辯錨點,不擅自加防護。

### 2.6.1 owner 繼承：認證即合併（DD-11）

**問題**：同一人有多裝置（手機、電腦各一把 token、各一個 uid），其中一台是行程
owner 且設了 email。另一台以**同名**進入該行程時,應能認證接管——而不是變成第二個
陌生的同名者。

**challenge 是提示,不是閘**。`ownerTakeoverChallenge()`（`handlers.go`）在
`proposalView` 掛一個 `omitempty` 的旗標,唯一為 true 的臂是**同名 + 不同 uid +
owner 有 email**。前端 `maybeOwnerTakeover()` 在 `openDetail` 據此彈一個**可關**的
modal（Q4:提示式,關掉照看照投,未認證期間投的票算當下 uid）。**用 `omitempty`
是因為**:一個在每則回應都 present-and-false 的欄位,會誘導前端讀成「檢查跑過且放行」
——而那在「偵測根本沒接線」的 build 上也是 false,兩者靜默同輸出。

**真正的授權閘是 magic-link consume,不是 challenge**。`MergeUsers` 全庫唯一生產
caller = `verify.go` 的 confirm 路徑,只在合法 magic-link token 兌現成功後觸發。
兩個 uid 都來自 **server-side `pendingMerge`**（DD-8:`fromUID` 在**請求當下**從
cookie 讀,`uid` 由 name+email 解析）——confirm 只帶 opaque token,**uid 從不過線**,
故改 query 參數無法選誰被併,**arbitrary uid 併吞在結構上不可達**。

**合併是完整吸收,不是只搬 token（Q2(c)）**。認證通過代表 B 就是 A ⇒ `MergeUsers`
把 B 的 **token + proposal ownership + option 作者 + 票**全部併進 A,**A 存活、
owner 恆為 A**。只搬 token 會讓 B 名下的提案停在 `OwnerUID=B` 而無任何裝置能認證成 B
——正是要消滅的孤兒。這仍在**單一 `EvUserMerge` 事件**內完成,**不是**先前設計過又
砍掉的 standalone `EvProposalTransferOwner`。

**票去重:留 `CreatedAt` 較新那張,跨全部行程**（一人一票是全域 invariant,不是
per-proposal）。合併 A 較新則留 A、B 較新則留 B、同時間留 incumbent(不依賴 map 順序)。

**email 唯一性:新寫入強制,replay 無條件豁免**。`SetEmail` 的 commit validate
closure 內掃 roster,同 address 不同 uid → `ErrEmailTaken`（`emailTakenByOtherLocked`,
大小寫正規化）。**但 replay 路徑（`applyEvent`）不套此檢查**——legacy log 可能在規則
存在前就寫了兩個同 email 身分,replay 必須無條件接受,否則舊日誌啟動失敗。這使
「兩身分同名同 email」在寫入路徑不可達、在 replay 路徑仍可達,正是
`TestUserByNameAndEmailRefusesAmbiguity` 用 replay 建 fixture 的原因。

`ErrEmailTaken` **有** HTTP 映射（`writeSetEmailErr` helper → `EMAIL_TAKEN` 409）:
三個 `SetEmail` 呼叫點共用它,否則各自 inline 會讓「換個 address 就能解決的衝突」在
某條路徑落成 500 `PERSIST_FAILED`（伺服器故障）——這正是「兩處對同一 store error
答不一樣」要防的。

`mergeUsersLocked` / `mergeEvictionsLocked` 為 **live 與 replay 共用 body**,
eviction 決定由事件攜帶（同 §2.5「淘汰決定由事件攜帶,replay 不重算」的紀律,避免
replay 分歧）。`MergeUsers` 走單一 commit,`writeMu` 下原子決定並寫入,8 並行合併
搶同一 owner 只 1 勝、owner 恆 A（`-race` 綠）。

### 2.7 rename 姓名唯一原子化（DD-3）

`RenameUser`（`store.go:362-385`）的唯一性檢查在 **commit validate closure 內**
（掃全 roster,同名不同 uid → `ErrNameTaken`）,與寫入共享同一 `writeMu` critical
section,與 `CreateUserWithSuffix` 同 pattern。HTTP 層只把 `ErrNameTaken` 映射成 409,
**不自行先問 `NameTaken`**——後者是 check-then-act,兩次獨立上鎖在並行 rename 下會讓
8 個同名全部寫入成功（VANS round-1 實測 8/8 繞過）。原子化後 8 並行 → 1 winner +
7×409,守住「前端名單姓名唯一」（使用者第 4 點要求）。

---

## 3. 授權邊界（A6，唯一的安全邊界）

`auth.Authorize` 是純函式：無 I/O、無時鐘、無變更。前端隱藏按鈕是便利**從來不是
保護**——每一條真正重要的測試都繞過 UI 用 curl 打。

### 裁決順序是承重的

```
1. 身分閘（未登記 ⇒ 拒絕所有寫入）
2. 提案存在性（ProposalOwner）
3. decide / repid 早退  ← 必須在凍結閘之前
4. 凍結閘（已定案 ⇒ 凍結投票與可選項編輯）
5. 各動作規則
6. default DENY
```

**第 3 步在第 4 步之前是硬性的**：若凍結閘先跑，唯一能解除凍結的動作會被凍結閘
自己擋下，提案永久凍結且不報錯。`repid` 同理——已定案的提案其分享網址一樣可能
外洩，撤銷手段不能在最需要它的狀態下失效。

**可選項編輯的三分支順序也是承重的**（TV-4）：提案人分支必須在作者分支之前，否則
會錯誤拒絕提案人；而該缺陷在「提案人剛好也是作者」的測試中完全不可見。

`OthersVoteCount` 排除作者自己的票——用總數會讓 TV-2 與 TV-3 無法區分。

**default DENY**：日後新增的 action 常數若忘了路由，落到 default 被拒絕。遺漏的
失敗模式必須是「擋住」，永遠不是「大開」。

---

## 4. 對外介面

### 4.1 兩個 listener，不是一個

```
-addr        對外（預設 127.0.0.1:8080）   應用 API + 前端資產
-admin-addr  管理（預設空＝關閉）           /api/admin/*
```

**admin 路由完全不註冊在對外 mux 上**——route 的「不存在」才是保證。這是 2026-08-05
修掉的現正生效破口：gateway 同機轉發使 `RemoteAddr` 恆為 `127.0.0.1`，
`requireLoopback` 恆為 true，區網任何人都能列舉全部提案與使用者。

那段程式的註解寫的「RemoteAddr 是核心視角、無法偽造」**本身完全正確**——是部署把
它的前提整個反轉了，而程式裡沒有任何東西能觀察到這件事。單元測試也抓不到：httptest
直連時 `RemoteAddr` 是真實 peer，8 個偽造標頭案例全部正確被擋。**測試證明了邏輯是
對的，而缺陷不在邏輯，在部署換掉了邏輯的前提。**

不能靠「更好的判準」修——判準執行時代理早已抹掉呼叫者身分。只能靠結構性移除。

`-admin-addr == -addr` 時**拒絕啟動**並說出理由。

### 4.2 端點

| 方法 | 路徑 | 說明 |
|---|---|---|
| GET | `/api/health` | 自報 `assets: embedded \| disk (web/)`、`pendingCompact` |
| GET/POST | `/api/me`, `/api/register` | 身分（含復原碼接回 + 撞名三路分流 2.6） |
| POST | `/api/verify-email` | 觸發 magic-link 寄送（uniform response 防 oracle） |
| GET/POST | `/api/verify-email/confirm` | 點連結確認綁定（GET 供 mail client） |
| POST | `/api/identity` | 齒輪 modal 改 name/email（DD-6,身分只來自 cookie） |
| POST | `/api/proposals` | 建立 |
| GET/PATCH/DELETE | `/api/proposals/{pid}` | 單一提案 |
| GET | `/api/my/proposals` | **只有這一個列表**，範圍是「我建立或表態過的」 |
| POST | `/api/proposals/{pid}/decision` | 定案／解除（空 optionId＝解除） |
| POST | `/api/proposals/{pid}/repid` | 更換分享網址（舊 pid 立即 404） |
| POST/PATCH/DELETE | `.../options[/{oid}]` | 可選項 |
| POST/DELETE | `.../votes[/{oid}]` | 表態 |
| GET | `.../stream` | SSE |
| POST | `.../cursor` | 游標 |

**R8：不存在任何可枚舉全部提案的對外端點。** 知道 pid 就是入場資格，所以分享網址
本身就是憑證——沒有密碼可改、沒有 session 可撤，`repid` 是這個設計唯一容許的撤銷
方式，且舊 pid 硬 404 不轉址（轉址會讓外洩網址繼續可用卻回報已撤銷）。

### 4.3 SSE 線材格式

**所有事件一律送裸 payload，不送 `{type,data}` 信封。** 事件種類已由 SSE 的
`event:` 欄位承載。

2026-08-05 修掉的真缺陷：snapshot 送裸的、其餘送包的，同一條串流兩種格式。前端讀
`d.uid` 恆為 `undefined` ⇒ **游標整個 M1 期間從未顯示過**、**presence 在有人加入時
反而清空整份名單**（方向與正確行為相反且不報錯）。

既有測試抓不到的原因值得記死：斷言寫的是 `strings.Contains(data, "x":0.25)`，而包了
信封的形狀**同樣含這個子字串**。**子字串存在性回答的是「有沒有出現」，從來不是
「結構對不對」。**

---

## 5. 觀測點（debug 進入點）

| 訊號 | 位置 | 回答什麼問題 |
|---|---|---|
| `/api/health` 的 `assets` | `httpapi.go` | 服務的是 embed 還是磁碟 `web/`？改檔為何不生效？ |
| `/api/health` 的 `pendingCompact` | 同上 | 壓實計數器現值；重啟後應歸零 |
| `/api/health` 的 `tokensTotal` | 同上 | 除以 `users` 即「平均每人幾台裝置」；貼近 5 代表上限太緊 |
| `/api/health` 的 `usersAtTokenLimit` | 同上 | 上限 5 是否合適的**唯一實證**；恆為 0 代表上限從未生效 |
| `/api/health` 的 `evictionsTotal` | 同上 | 淘汰次數（程序內、重啟歸零）。淘汰是使用者可感知的傷害——某台裝置無聲登出 |
| `/api/health` 的 `candidateProbesTotal` | 同上 | 撞名候選/探測計數（process-local）。registrations 不動但此數攀升=有人在枚舉名字 |
| `/api/health` 確實**不**出 `pendingVerifications` | `httpapi.go:133` | 刻意移除：verify-email 命中會 mint pending、未命中不會,公開表大小=把 (name,email) oracle 從側通道洩出→ 無任何替代數字,觀測性移到 operator log |
| `grep user_add_token data/events.jsonl` | 磁碟 | 裝置史；`EvictedHash` 非空者即「誰擠掉了誰」。用前先以 `user_create` 驗證 grep 會非空 |
| `data/events.jsonl` 行數 | 磁碟 | 壓實有沒有真的截斷 |
| `data/snapshot.json` mtime | 磁碟 | 上次壓實時間 |
| `store.SetObserver` | `event.go` | 逐事件觀測窗口（測試用） |
| `store.SetCrashHookForControlGroup` | `event.go` | commit 兩步之間的崩潰注入點 |
| `store.SetUnsafeOrderForControlGroup` | `event.go` | 反轉 write-ahead 順序（控制組） |
| `journalctl --user -u teamdating` | systemd | 重啟、崩潰、SIGTERM |

**兩個控制組開關是刻意留在生產程式碼裡的**：沒有可對比的壞順序，一個通過的持久性
測試什麼都證明不了。生產路徑永不設定它們，且有測試釘住這件事。

---

## 6. 護欄（source guard：讀原始碼的測試）

行為測試看不見「接線根本不存在」，這三個看得見：

| 測試 | 守什麼 | 為何行為測試不夠 |
|---|---|---|
| `zerodeps_test.go` | `go.mod` require 為空 | 引入依賴不會讓任何功能測試變紅 |
| `httpapi/source_guard_test.go` | 前端 SSE 監聽清單涵蓋全部 `Ev*` 常數 | 漏一個不報錯：後端照送、前端沒掛、`onmessage` 對具名事件不觸發，畫面靜默不更新 |
| `store/source_guard_test.go` | 壓實中介層真的裝在 `New` 上 | 行為測試吃的是注入門檻，`New` 忘記安裝時它們會一起維持綠燈 |
| `store/multitoken_persist_test.go` 的 `TestUserCreateDataShapeIsFrozen` | `UserCreateData` 仍是單值 `TokenHash` | 改壞它**沒有任何可觀察的失敗**：舊日誌照樣解碼成功、replay 回報成功，只是所有人被登出 |

`httpapi` 那條在 6.9 **抓到我自己的遺漏**——新增 `EvProposalRepid` 後前端清單沒
跟上，測試立刻轉紅。多 token 改動時它**再次抓到同一類遺漏**：三個新 `user_*_token`
常數加進後端後前端沒掛，測試立刻指名缺哪三個。

### 已提交的控制組入口（不是待清理的殘留）

`SetUnsafeOrderForControlGroup` / `SetUnsafeCompactOrderForControlGroup` 之外，
多 token 改動再加一個 `loadSnapshotV2Only`——一個**只有 v2 路徑**的解碼器，供 TV-8
證明「不處理 v1 就載入 v1 檔」真的會失敗。

**它是提交進去的，不是臨時改壞再改回。** 後者讓正確性取決於有人記得復原，而這個
repo 已經為那個窗口付過兩次帳（一次跑到未復原的 bytes、一次 `git checkout --` 把
未 staged 的修復一起洗掉）。提交進去的控制組沒有東西要復原，session 中斷也留下
乾淨的樹。

---

## 7. 前端（零依賴 ES module）

`web/index.html` + `web/app.css` + `web/app.js`，無建置步驟、無打包器。

**渲染契約**：SSE 增量事件 → 重取快照到記憶體 → **節點級 patch**，絕不
`innerHTML=''` 整片重建。日曆與可選項清單都保留 `scrollTop`（5.8 承重；控制組是把
`patchDetail()` 換成 `renderDetailFull()`，scrollTop drift 0 → 101）。

**兩處刻意用指標語意**（`myWeight`、`voteReq.Weight`），同一條理由：不行的權重是
**0**，而非指標會讓「沒帶這個欄位」與「明確表示不行」共用同一個值。

---

## 8. 部署

- 資料目錄 **必須是真實磁碟**（`~/.local/share/teamdating`），不可用 `/run/user/*`：
  tmpfs 開機清空，且 **`fsync` 在 tmpfs 上是 no-op** —— 現有測試證明的是**順序契約**
  不是斷電持久性。
- `Restart=always` 而非 `on-failure`：最常見的意外停止是 kill 錯 pid，那是乾淨離開。
  **2026-08-05 已實際遷移上線**：`systemctl --user teamdating.service`，資料在
  `~/.local/share/teamdating/data`（ext4，非 tmpfs）。`kill -9` 實測 pid 8130→8585
  自動拉起、資料完整。
- **WSL 的 systemd 缺 `/run/credentials`**，主 unit 的四條 namespace 保護
  （`ProtectSystem`/`ProtectHome`/`PrivateTmp`/`ReadWritePaths`）各自單獨開啟都會
  以 `226/NAMESPACE` 失敗。主 unit 保留安全預設，環境限制走
  `deploy/teamdating.service.d-wsl-override.conf`。詳見 `deploy/README.md`。
- 反向代理 **必須** `proxy_buffering off` + `proxy_read_timeout 1h`，否則 SSE 長命
  連線被 60s 預設砍斷，表現成「每分鐘閃一下」而不是明確錯誤。
- **本部署的 gateway 實測不送任何 `X-Forwarded-*`**（`nc -l` 攔下原始位元組確認）。
  任何「從 XFF 取得客戶端 IP」的邏輯會編譯過、測試過，然後在生產把全世界放進同一個
  桶。已 file 給 opencode 值星官（對方獨立覆核後受理）。限流因此改用 token hash 當鍵。

---

## 9. 已知敞口（未解決，不得當成已解決）

> ★ 本節於 2026-08-05 全面重寫。原本四條裡有 **三條與實測不符**，而其中兩條是
> **寫反的**——不是漏寫，是描述指向了錯誤的方向。這比缺少條目更貴：**一句寫反的
> 敞口描述會停止讀者尋找真正的敞口。** 修正過程與證據見各條目下的 BR。

- **`fsync` 斷電語意仍未證，但已不是「完全沒量」**（部分收斂）。
  - 已證（`internal/store/durability_ext4_test.go`，四條各帶控制組）：受測目錄確實在
    ext4、`append()` 真的發出 fsync 系統呼叫（strace 差分 10，拿掉生產碼 `s.log.Sync()`
    → 差分 0 且 FAIL）、該 fsync 在 ext4 上有真實成本（約 1200x vs tmpfs 的 1.3x）、
    順序契約在真實磁碟上成立。
  - **仍未證**：拔電後資料還在。SIGKILL 不清 page cache，使用者空間**原理上**做不到；
    本機 WSL2 kernel 無 `dm-flakey` / `dm_log_writes`。四條合起來不等於斷電語意。
  - **原描述「現有崩潰測試都跑在 tmpfs 上」的根因寫錯了**：不是 `t.TempDir()`——本機
    `/tmp` 被 systemd `PrivateTmp=` bind 到 ext4。真正落 tmpfs 的是 `store_test.go:14`
    的 `tempDir()` helper 寫死 `XDG_RUNTIME_DIR`。結論對、根因錯。
  - 裁示：`tempDir()` **不改**（整包測試變慢是確定成本，新檔已補上缺口並與既有測試並存）。
  - 未覆蓋：`snapshot.go` 的 `writeFileSync` 仍只有 tmpfs 覆蓋；多行程並發寫同一 log。

- **單點部署**：無水平擴展設計，`Store` 是行程內記憶體權威。（唯一未變動的原條目。）

- **~~無 CSRF token / 同站攻擊面未評估~~ → 已評估，且找到的不是 CSRF 而是憑證外洩**
  （`BR_20260805_cookie_path_leaks_token_to_sibling_apps`，commit `02e2180` 已修）。
  - 拓撲事實：本服務部署在**路徑多工的反向代理**之後，多個彼此無關的 sibling app
    以各自前綴掛在**同一個 origin** 上（`/<sibling-app>`…）。cookie 原本寫死
    `Path=/`，於是瀏覽器把本服務的 bearer token **主動附加到每一個鄰居**——實測
    `curl -b jar .../<sibling-app>/api/x` 送出 `td_token`。
  - `HttpOnly` 在此無效且極易被誤讀：它擋的是頁面 JS **讀取** cookie，不擋瀏覽器
    **發送**。鄰居的伺服器端直接收到 token，一個把請求標頭寫進 log 的正常應用就
    足以把 400 天有效的憑證落盤。
  - 已修：`Path` 收斂到 `-base` 前綴，並發 `Path=/` + `Max-Age=0` 清掉既有的寬作用域
    cookie（兩者是不同 cookie 會並存，不清會續漏 400 天而修復看起來已落地）。
  - **殘留**：同 origin 下 `SameSite=Lax` 對「鄰居應用發起的請求」仍不構成保護，
    因為那不是跨站。目前靠 `Path` 隔離而非 token 校驗——若未來新增能被 GET 觸發的
    變更操作，這條會重新成為真敞口。

- **~~cursor / presence 無速率限制：限流只掛在寫入路徑~~ → 寫反了，實際是限錯桶**
  （`BR_20260805_cursor_traffic_drains_the_user_write_budget`，commit `354cc2c` 已修）。
  - 實際情況與原描述相反：`POST /cursor` **在**寫入路徑上，且與真正的寫入**共用同一
    個桶**。前端每 60ms 送一次（≈16/秒）對上 5/秒 burst 20——實測游標第 20 次就被擋，
    之後使用者的投票拿到 429。症狀間歇出現且與使用者做的事沒有因果關聯。
  - 既有測試全綠是因為**沒有任何一條同時走這兩條路徑**，各自的斷言都成立。缺陷只
    存在於交互上。
  - 已修：`isTransientFanout` 白名單（新增的變更路由預設計入寫入桶，被保護是預設
    不是遺漏），游標走 30/秒 burst 60 的獨立桶，且**仍受限**（免除等於製造免費扇出
    放大器）。
  - **殘留**：SSE 訂閱本身（`GET /events`）不限流，一個客戶端可開多條長連線。

---

_最後同步：2026-08-08（owner 繼承 email-authenticated device merge 交付並部署 live：新增 §2.6.1 owner 繼承合併、§2.4 事件集合加 `user_merge`。核心=認證即合併(DD-11/Q2(c)):challenge 提示不阻擋、真閘為 magic-link consume(uid 不過線、arbitrary uid 併吞不可達)、B token+ownership+票完整吸收進 A(A 恆 owner)、票去重留較新跨全行程、email 唯一性新寫入強制+replay 豁免、`ErrEmailTaken`→409 共用 helper。親手驗收 store 94/httpapi 127/root 全綠 + VANS-CLEARED round-1；commit aa31773 merge master 98906f7 換 binary 部署三證換版(/proc/exe sha e3e8122a、pid 6050→75507、資料零損 174 行 head-sha 一致)。比對依據：逐檔讀 diff 實際接線 + VANS 獨立稽核，非憑記憶）。先前同步：2026-08-07（identity access control + email magic-link 認證，§2.6/§2.7/mailer/user_set_email）。_
