# 白皮書 v1.0 → v1.1 修補清單

**用法：** 把這份內容貼在 `StateFlow_Whitepaper_v1_0.md` 的**最前面**，檔案更名或標記為 v1.1。逐條消化進正文之後，把該條從這裡刪掉。全部消化完，這份清單只剩最後的「版本更新內容」一節，移到檔尾。

**修補來源：** `STATE_SNAPSHOT.md`（Session 22，`main` @ `adb24a4`）與 `BEHAVIOR_MATRIX.md` v1.1 的裁定。

**分類：**
- **【失實】** 白皮書寫的事情現在是假的 —— 最高優先，會誤導讀者
- **【缺漏】** 已實作但白皮書完全沒提
- **【新裁定】** 本輪新定的規格，白皮書尚未有

---

## 第一優先：§18 Temporary Design Registry 【失實】

> **這是全篇失實最嚴重的一節。** 八項登記缺口中六項已關閉，白皮書仍寫著它們是缺口。任何讀者（包括面試官）都會嚴重低估專案完成度。

整張表改寫為：

| # | 項目 | 狀態 | 現在的行為 |
|---|---|---|---|
| 1 | 全史傳輸 | ✅ **已關閉** | 單筆 `HistoryEntry.Output` 超過 2 KB（marshaled）轉為小型指標物件；**保留的** Output 累計上限 50 KB，自最新往回走，超出者整筆丟棄 `Output`。**注意：50 KB 只計 Output 位元組，不含 name/status 等結構開銷** |
| 2 | 遲到結果回收 | ⬜ **仍開放** | 超時判決後抵達的成功一律丟棄。白皮書自身標記為 "2+"，本階段刻意不做 |
| 3 | planner 重試計數在記憶體 | ⬜ **仍開放** | crash 歸零。因 planner 呼叫無副作用故安全 |
| 4 | storage 孤兒等重啟 | ✅ **已關閉** | 行程內 sweeper，預設每 30 秒掃一次（`SWEEP_INTERVAL_SECONDS` 可覆寫）。storage 短暫故障不需重啟即可回收 |
| 5 | `retry_after_seconds` 被忽略 | ✅ **已關閉** | 生效延遲 = `max(worker 回報值, 系統預設)` |
| 6 | main.go 硬編組裝 | ✅ **已關閉** | `RETRY_MAX_ATTEMPTS` / `RETRY_DELAY_SECONDS` / `SWEEP_INTERVAL_SECONDS` 三個環境變數，預設等同原硬編值，**格式錯誤 fail-fast** |
| 7 | init-only migration | ✅ **已關閉** | 改用 `golang-migrate`；`migrations/000001_initial.{up,down}.sql`，於 `main.go` 啟動時、`RecoverRuns` 之前套用 |
| 8 | 無 healthcheck | ✅ **已關閉** | `GET /healthz`（ping Postgres，3s，200/503，純讀）＋ `stateflow healthcheck` CLI 子指令（distroless 無 shell 亦可用）；Dockerfile 與 compose 皆已接上 |

**建議在表格上方加一句：** 「本表是承諾清單的補集。截至 v1.1，原八項中六項已關閉；保留已關閉項目與其實作方式，是為了讓讀者看見缺口如何被逐一收斂，而非刪除歷史。」

---

## §12.2 RunState 與全史傳輸 【失實】

**現況文字：** history「carries each step's full output」，並附一段「Registered as the system's weakest link」的引言區塊。

**改為：** history 依 `seq` 升冪，**每筆 output 受兩層大小上界約束**：

1. 單筆 `Output` 超過 2 KB（marshaled）→ 替換為小型指標物件。
2. 自最新往回走，**保留的** Output 累計超過 50 KB → 更早的項目整筆丟棄 `Output`（保留 name 與 status）。

刪除「weakest link」引言區塊，改為一句：「歷史裁剪由系統機械執行，planner 不需協商；被裁掉的 output 可經 `GET /runs/{run_id}` 取回。」

**必須明說的兩個細節（否則使用者會誤判）：**
- 50 KB 上限**只計 Output 位元組**，不含 name/status/結構開銷，所以實際 payload 會略大於 50 KB。
- 走訪方向是**最新優先**——舊步驟先被丟。方向寫反會靜默反轉整個功能的語意。

---

## §13.2 Worker 回報 —— `retry_after_seconds` 【失實】

**現況文字：** 「accepted but ignored in the MVP; the field is reserved」

**改為：** 「已生效。生效重試延遲 = `max(retry_after_seconds, 系統預設延遲)`。worker 要求更長的等待會被採納；要求更短不會 —— 系統預設是地板。0 或負值等同未提供。」

---

## §6 Timeout Doctrine 【失實 + 缺漏】

**失實：** 「retry delay | fixed 5 s (MVP)」——現在可由 `RETRY_DELAY_SECONDS` 覆寫。

**改為：** 「retry delay | 預設 5 s，可由 `RETRY_DELAY_SECONDS` 環境變數覆寫（行程層級，非 per-workflow）；worker 可經 `retry_after_seconds` 要求更長，取大值。」

**缺漏一 —— 三個數字的分界。** 建議新增一個小區塊，因為這是實務上最常見的理解錯誤：

> **三個數字，職責互不重疊：**
> - **timeout** —— 一個 attempt 的生命上限：等多久之後宣判它失敗
> - **retry delay** —— 宣判失敗後、建立下一個 attempt 前的冷卻
> - **retry limit X** —— 累積幾次失敗之後進 DLQ
>
> 最壞情況下，一個 step 從開始到進 DLQ 約需 `X × (timeout + retry delay)`。使用者若只用 `X × timeout` 估算，會低估等待時間。

**缺漏二 —— X 的覆寫層級。** §6 完整描述了 timeout 的三層繼承，卻沒說 X 有幾層。加上：「retry limit X 支援兩層：step（StepSpec）覆寫 workflow 預設。與 timeout 對稱。」（此為本輪新裁定，見下方 §12.3。）

---

## §9 Storage 與 §8.3 Recovery 【缺漏 + 新裁定】

**缺漏 —— 行程內 sweeper 已實作。** §9 目前寫「Phase 2 proposal (adopted only if it stays simple)」，實際已完成。改為：

> **行程內 sweeper（已實作）。** 除了啟動時的一次性掃描，orchestrator 另有一個背景 sweeper，預設每 30 秒（`SWEEP_INTERVAL_SECONDS`）掃描一次 `run=RUNNING`。用途是：某個 run 的驅動 goroutine 因 storage 短暫故障而死亡時，**不需要重啟整個 orchestrator** 即可被回收。它與 crash recovery 走**完全相同**的認領路徑，因此可重入性與收斂性的論證原封不動適用。

**連帶影響 —— §21 的單副本限制變得更關鍵。** 建議在 §9 或 §21 加註：

> sweeper 使單副本限制從偶發問題變成持續問題：過去多副本只在「同時重啟」時衝突，現在每 30 秒就有一次重複掃描的機會。executor-ID ownership 因此從 nice-to-have 升格為多副本部署的前置條件。

**新裁定 —— 啟動時 fail-fast。** 加入：

> orchestrator 啟動時若無法連上 storage，**立即以非零狀態碼退出**，並在 stderr 印出明確指認為 storage 連線問題的訊息。不做靜默重試迴圈。整個系統建立在「storage 完好」的假設上；假設不成立時唯一正確的行為是大聲停下來。執行期斷線的錯誤日誌同樣必須可辨識為 storage 問題，不得被包裝成通用的 step 失敗。

---

## §12.3 Planner 決策驗收 —— 拆成兩層 【新裁定】

**現況：** 單一組「acceptance criteria」，全部是語法層。

**改為兩層，兩層都在 TX1 之前完成，兩層都歸 `malformed` 並消耗 planner 預算：**

**第一層 —— 語法驗收（即現有內容）：** 合法 JSON；含 `status`；`continue` 時含 `step.worker_url` 與 `step.mode`；除 JSON 外無任何散文或 markdown 圍欄。

**第二層 —— 語意驗收（新增）：**
- `step.name` 與本 run 已存在的 step **重名** → 拒絕。依據：history 已含每步 `name`，planner 看得到過去所有名稱，重名是它的錯。**絕不可讓它走到主鍵衝突。**
- `step.mode` 不是 `sync` 也不是 `async` → 拒絕。
- `step.worker_url` 語法不合法（非 http/https、空字串、無法解析）→ 拒絕。
- `step.retry_limit`（新欄位，見下）為負數或非整數 → 拒絕。

**必須同時說明的邊界：** worker_url **語法合法但執行期連不上**（DNS 失敗、連線被拒）**不是** planner 錯誤，而是 worker 失敗，走 attempt 預算。理由：它與 worker 掛掉在觀測上不可分辨，硬要區分會製造一條假分支。

**StepSpec 新增欄位：**

```json
"retry_limit": 3     // 選填。缺欄位或 0 ⇒ 繼承 workflow 的 retry_limit
```

---

## 新增章節：Config 與提交時驗證 【新裁定】

> 建議編為 §12.4 或獨立一節。這是白皮書目前完全沒有的規格領域。

**治理原則：** 寧可過度嚴謹，讓使用者抱怨我們不支援某件事；也不要過度寬鬆，讓他安靜地出錯。所有可在提交時發現的錯誤，都不留到執行期。

**格式一致性：** 檔案端一律真正的 YAML（可寫註解）；HTTP API 端一律 JSON；DB 端 `planner_config` 仍為 JSONB。**不得存在副檔名與內容不符的檔案**（現況有 `.yaml` 但內容是 JSON 者，須修正 —— YAML 是 JSON 的超集，改判為 YAML 後既有內容仍合法解析，是零風險遷移）。

**輸入契約與儲存形狀分離：** API 接收的 body 有自己的 schema，驗證只針對這份 schema；DB 的落地形狀由 API 層正規化後寫入。**驗證器不得把 DB 欄位形狀當成輸入契約**——那會讓儲存細節洩漏到使用者介面，並使日後改 schema 變成破壞性變更。

**`POST /workflows` 的驗證規則（全部回 400，全部零副作用，全部先於 TX-W）：**

| 情境 | 回應 |
|---|---|
| `planner_type` 非 `static`／`http` | 400，訊息列出合法值 |
| `planner_type=http` 缺 URL；`static` 缺步驟表 | 400，訊息指出缺哪個欄位 |
| **交叉污染**：static 的 config 出現 http 專屬欄位，或反之 | 400，訊息明說「此欄位不屬於 planner_type=X」 |
| `planner_config` 出現未知欄位 | 400（嚴格模式） |
| **頂層**出現未知欄位 | 400。**與上一條是兩個不同層級的檢查** |
| 型別錯誤（`retry_limit` 給字串、`planner_config` 給陣列） | 400。**不得靜默轉型**（不把 `"2"` 當成 2） |
| static 步驟表中兩個 step 同名 | 400。static planner 在執行期「構造上不會失敗」的保證，正是靠這一條在提交時成立 |
| static 步驟中缺 `name`／`worker_url`／`mode`，或值域錯誤 | 400，訊息指出第幾個步驟的哪個欄位 |
| `retry_limit` 缺失／非整數／< 1；timeout 為負 | 400 |
| **`name` 重複** | **允許，正常建立。** name 僅為顯示標籤，不加 UNIQUE；唯一識別靠 `workflow_id` |

**Planner 對自身契約的認知：** http planner 必須有一份可交付使用者的明確格式規格（RunState 進、StepDecision 出，含兩層驗收）。輸出不符契約一律歸 `malformed`，**絕不嘗試猜測修正**——不剝 markdown 圍欄、不容錯解析、不補預設值。容錯解析會讓錯誤靜默通過，違反本節治理原則。

---

## §14.1 Schema 【新裁定】

`workflows` 表新增（或確認已存在）兩個一級欄位：

```sql
retry_limit             INT NOT NULL DEFAULT 3  CHECK (retry_limit >= 1),
default_timeout_seconds INT NOT NULL DEFAULT 60 CHECK (default_timeout_seconds > 0)
```

**理由（必須寫進白皮書，因為這是可讀性論證）：** 這兩個值原本藏在 `planner_config` JSONB 裡，導致 `\d workflows` **完全看不出系統有這兩個旋鈕**。它們是 workflow 層級的執行參數，與「怎麼決定下一步」無關。移出後 `planner_config` 的職責收窄為「該 planner type 專屬的設定」：`http` ⇒ `{url}`，`static` ⇒ `{steps}`——這正是嚴格驗證能有精確合法鍵集合的前提。

**per-step retry limit 不新增 DB 欄位：** 存於 `steps.decision` JSONB 的 StepSpec 內。TX3 判斷時本來就會讀 decision，零額外查詢。**判定當下用的是落盤時的 X**——與 §4.1「DLQ 是依當時配置做出的判決」一致。

**X 的解析優先序：** step（`StepSpec.retry_limit`）> workflow（`workflows.retry_limit`）。不得再從 `planner_config` 取值。

**Migration 方式：** 專案已使用 `golang-migrate`（§18 #7）。**本變更必須是新增的 `migrations/000002_*.{up,down}.sql`，up 與 down 都要寫。** 不得就地改寫 `000001`。

---

## §11 DLQ 與 Replay 【新裁定】

**Replay 冪等性：** 對同一 DLQ entry 連續呼叫 replay，第一次的 TX5／TX6 在單一交易內把 run 帶離 DLQ，**這本身就是冪等閘門**。第二次檢查 run 現況：非 DLQ ⇒ **409 Conflict**，訊息須指出目前的實際狀態（RUNNING／DONE），零狀態效果。

> **判斷依據是 run 的現況，不是 DLQ entry 是否存在。** DLQ 那一列是歷史紀錄，永不刪除，所以它必然還在。

**現況表與歷史表的職責分界（建議也在 §14.1 重述）：**
- `runs`／`steps` 記錄**當下狀態**，每個 run／step 恰好一列。
- `dead_letter_queue` 記錄**歷史事件**，同一 run 可累積多列（replay 後再失敗即產生第二列）。
- **「這個 run 現在是不是 DLQ」的唯一判斷依據是 `runs.status` 與 `steps.status`，絕不是 DLQ 表裡有沒有列。** 任何讀取路徑若只查 DLQ 表就下結論，即為缺陷。

**`GET /dlq` 的語意：** 預設只列出**目前 `run.status='DLQ'`** 的項目（真正待處理的人工佇列）。已被 replay 帶離 DLQ 的歷史 entry 不出現在預設列表，否則列表會被歷史殘留污染。分類由組合表推導、不新增欄位：`run=DLQ ∧ last_step=DLQ` ⇒ worker 側（`step_id` 非 null）；`run=DLQ ∧ last_step=DONE`（含無 step）⇒ planner 側（`step_id` 為 null）。

---

## §10 回報處理 —— 冷卻窗口 【新裁定】

在 §10 現有的三條之後加入：

> **重試延遲窗口內抵達的任何回報一律不生效。** TX3 commit 之後、TX4 commit 之前，`current_attempt_id` 指向的 attempt 狀態不是 `RUNNING`，CAS 必然不匹配 ⇒ 200 ACK、零狀態效果。這是「超時後的成功被拒」的一般化：**冷卻期間的任何回報都不承認、不採用、不寫入。**
>
> **此規則不得靠應用層判斷實現。** 必須由 CAS 的 `AND status='RUNNING'` 自然達成。若實作中出現「檢查現在是不是冷卻期」的分支，即為缺陷——那是第二個判斷來源，遲早與 CAS 分歧。同理，callback 抵達時 run 已是 DLQ 的情況也不需任何額外檢查。

---

## 新增：可觀測表面 【缺漏】

> 白皮書目前完全沒有這兩個端點。建議加在 §13 之後或與 API 列表同節。

**`GET /healthz`** —— ping Postgres，3 秒逾時，200／503，**純讀無寫入**。另有 `stateflow healthcheck` CLI 子指令（HTTP-GET 自身的 `/healthz`，離場碼 0／1），使 distroless image 在沒有 shell 的情況下也能做 healthcheck。Dockerfile 的 `HEALTHCHECK` 與 compose 的 `healthcheck:` 皆呼叫此子指令。**這是 Kubernetes readiness gating 的前置條件，現已具備。**

**`GET /ui`** —— 單一內嵌 HTML（Go `embed`），無外部 CDN，主題感知。**純讀**：只呼叫 `GET /runs/{id}` 與 `GET /dlq`，**頁面中不存在任何寫入路徑**。定位：耐久性是沉默的美德，不出事時看不見；這個頁面讓 frontier 的推進與 recovery 的回收變得可見。

---

## §21 Roadmap 【失實】

Phase 1.5 與 Phase 2 大部分已交付，Roadmap 仍寫成未來計畫。

- **Phase 1.5：** CI ✅、`/healthz` ✅、README 重寫 ✅、狀態 UI ✅。**ghcr.io 發布與 demo storybook／影片明確延後**（owner 決定）。
- **Phase 2：** 登記項 #1／#4／#5／#6／#7 全部 ✅；#2（遲到結果回收）明確不做。
- **Phase 3 不變**，但加註 sweeper 使單副本限制更為關鍵（見上方 §9）。

---

## 【建議加入】Q&A 補三題

現有 17 題可加：

**Q18. timeout、retry delay、retry limit 三個數字差在哪？**
timeout 是一個 attempt 的生命上限；retry delay 是失敗判決到下一次嘗試之間的冷卻；retry limit X 是累積幾次失敗進 DLQ。最壞情況約 `X × (timeout + delay)`（§6）。

**Q19. 為什麼 config 錯誤要在提交時就拒絕，而不是執行時？**
執行期發現的 config 錯誤會消耗預算、產生 DLQ 項目、浪費操作者的分診時間，而它在提交當下就百分之百可判定。寧可過度嚴謹讓使用者抱怨，也不要過度寬鬆讓他安靜出錯（§12.4）。

**Q20. 為什麼 `retry_limit` 從 `planner_config` 搬到獨立欄位？**
它是 workflow 層級的執行參數，與「怎麼決定下一步」無關；藏在 JSONB 裡使 `\d workflows` 看不出系統有這個旋鈕。搬出後 `planner_config` 的職責收窄為「該 planner type 專屬的設定」，嚴格驗證才有精確的合法鍵集合（§14.1）。

---

## 版本更新內容（消化完成後移到檔尾）

**v1.0 → v1.1**

*失實修正（白皮書落後於實作）：*
- §18 登記表：八項中六項標記為已關閉，補上各自的實作方式
- §12.2：全史傳輸已上界化（2 KB／筆、50 KB 累計、最新優先）
- §13.2：`retry_after_seconds` 已生效，取大值規則
- §6：retry delay 可由環境變數覆寫，非固定值
- §9：行程內 sweeper 已實作，非提案
- §21：Phase 1.5／2 交付狀態更新

*新增規格：*
- §12.3：決策驗收拆為語法／語意兩層；StepSpec 新增 `retry_limit`
- 新章節：Config 格式與提交時驗證（嚴格模式、輸入契約與儲存形狀分離）
- §14.1：`workflows` 新增 `retry_limit`／`default_timeout_seconds` 一級欄位；X 兩層解析
- §11：replay 冪等（409）、`GET /dlq` 預設過濾、現況表與歷史表職責分界
- §10：重試延遲窗口內回報一律不生效，且不得靠應用層判斷實現
- §9：storage 啟動時 fail-fast

*新增章節：*
- 可觀測表面：`GET /healthz`、`GET /ui`
- Q&A 增補三題（Q18–Q20）
