# Claude Code Prompt B —— 依規格實作

> **執行環境：** 在 repo 內執行。若與 Prompt A 並行，**必須使用獨立的 git worktree**（Sessions 11/12 曾因共用 checkout 導致 HEAD 被抽掉）。
>
> **權限紅線：** `test/acceptance/` 與 `spec/BEHAVIOR_MATRIX.md` 對你**唯讀**。你不得建立、修改或刪除其中任何檔案。若你認為某個測試或某條規格是錯的 —— **停下來回報，不要自己改**。

---

## 你的角色

你在實作 **StateFlow** 的一批規格變更。`spec/BEHAVIOR_MATRIX.md` 是權威行為規格，`docs/StateFlow_Whitepaper_v1_0.md` 是架構背景（**注意：白皮書多處已落後於實作，矩陣 K 節列出了落差**）。

另一個平行的 session 正在**看不到程式碼**的情況下，依同一份矩陣撰寫驗收測試。你們的共同真相來源是矩陣，不是彼此。**不要去猜它會怎麼寫測試，照規格做。**

---

## 動工前必做的三件查核

**這三件會決定你要做什麼，不要憑假設開工。**

1. **`workflows` 表現在到底長什麼樣？** 執行 `\d workflows`。`retry_limit` 與 `default_timeout_seconds` 是獨立欄位，還是 `planner_config` JSONB 裡的鍵？快照的 CONFIRM 值顯示 `default_timeout_seconds` 可能已存在但 `retry_limit` 仍在 JSONB 內 —— **實測，不要相信文件**。
2. **migration 現況。** 專案已使用 `golang-migrate`（Session 22），檔案是 `migrations/000001_initial.{up,down}.sql`。確認目錄實際內容與 `main.go` 的套用時機。
3. **既有的 config 驗證有多少？** `POST /workflows` 現在拒絕什麼、接受什麼？這決定 N 節是新增還是補強。

**把這三件的實測結果放在報告最前面。**

---

## 工作項目

依序做，每項獨立可驗證。

### 1. Migration 000002 —— schema 變更

**做法已變更：** 舊的 session 規則（就地改寫 `001_initial.sql`、不引入 migration 工具）**已於 Session 22 作廢**。

- 新增 `migrations/000002_*.{up,down}.sql`，**up 與 down 都要寫**
- **不得就地改寫 `000001`** —— 那會使已套用該版本的資料庫與檔案不一致
- 內容：`workflows` 加 `retry_limit INT NOT NULL DEFAULT 3 CHECK (retry_limit >= 1)`；若 `default_timeout_seconds` 尚不存在，一併加上 `INT NOT NULL DEFAULT 60 CHECK (> 0)`
- **既有資料的搬遷：** up migration 需把現有 `planner_config->>'retry_limit'` 的值搬進新欄位（無值者用預設）
- 對應矩陣：N-20、N-20a、N-21

**驗收：** `docker compose down -v && up -d`，貼出 `\d workflows`；另測 down migration 可執行。

### 2. Config 提交時驗證（矩陣 N 節）

新增一個獨立的驗證層，**全部先於 TX-W，全部零副作用**（驗證失敗不寫任何 DB 列）。

**兩層未知欄位檢查 —— 這是最容易漏的：**
- 頂層合法欄位：`name`、`planner_type`、`retry_limit`、`default_timeout_seconds`、`planner_config`。其餘 → 400（N-18）
- `planner_config` 內合法欄位：`http` ⇒ 僅 `url`；`static` ⇒ 僅 `steps`。其餘 → 400（N-07）
- **交叉污染**（static 的 config 出現 `url`，或反之）→ 400，訊息明說「此欄位不屬於 planner_type=X」（N-06）

**其餘規則：** 型別錯誤不得靜默轉型（N-19）；static 步驟表重名 → 400（N-08）；步驟欄位缺失或值域錯誤 → 400 且指出第幾步（N-09）；`retry_limit` 非整數或 < 1 → 400（N-10）。

**`name` 重複是合法的**（N-17）—— 不加 UNIQUE 約束，唯一識別靠 `workflow_id`。

**錯誤訊息必須指名道姓：** 哪個欄位、為什麼、合法值是什麼。「invalid config」不合格。這整節的價值就在錯誤訊息的品質。

**輸入契約與儲存形狀分離（N.2 開頭原則）：** 驗證器針對 API body 的 schema，不是針對 DB 欄位形狀。

### 3. YAML／JSON 格式一致性（N-01 至 N-03）

- 副檔名 `.yaml` 的檔案，內容必須是真正的 YAML。現況有「叫 `.yaml` 但內容是 JSON」者，**修正之**（YAML 是 JSON 的超集，既有內容仍合法解析，零風險）
- 檔案端一律 YAML，HTTP API 端一律 JSON，DB 端仍為 JSONB
- 先 `grep` 出所有 `.yaml`／`.yml` 檔並判定，**在報告中列出你改了哪些**

### 4. Per-step retry limit 覆寫（N-24 至 N-28）

- `StepSpec` 加 `retry_limit`（選填）。缺欄位或 0 ⇒ 繼承 workflow
- **不新增 DB 欄位** —— 存在 `steps.decision` JSONB 內，TX3 判斷時本來就讀得到 decision
- X 解析優先序：step > workflow。**不得再從 `planner_config` 取值**（N-24）
- 負數或非整數 → planner 語意驗收失敗（`malformed`），不落盤（N-26）
- static planner 步驟表同樣支援，值域錯誤在提交時擋下（N-27）

### 5. Planner 決策的語意驗收（D-02b）

現有驗收只有語法層。新增語意層，**兩層都在 TX1 之前，都歸 `malformed` 並消耗 planner 預算**：

- `step.name` 與本 run 已存在的 step 重名 → 拒絕。**絕不可讓它走到主鍵衝突**（B-14）
- `step.mode` 不是 `sync`／`async` → 拒絕
- `step.worker_url` 語法不合法（非 http/https、空字串、無法解析）→ 拒絕（D-08）
- `step.retry_limit` 值域錯誤 → 拒絕

**邊界（不要做錯）：** worker_url **語法合法但執行期連不上** ⇒ **worker 失敗**，走 attempt 預算，**不是** planner 失敗（D-09）。理由：與 worker 掛掉在觀測上不可分辨。

### 6. Replay 冪等與 `GET /dlq`（F-08、F-10 至 F-13）

- 對同一 DLQ entry 重複 replay：檢查 **run 的現況**（非 DLQ ⇒ 409 Conflict，訊息指出目前實際狀態），**不是**檢查 DLQ entry 是否存在 —— 那一列是歷史紀錄，永遠都在
- `GET /dlq` 預設只列出目前 `run.status='DLQ'` 的項目
- worker 側／planner 側的分類由組合表推導（`last_step` 狀態 + `step_id` 是否為 null），**不新增欄位**
- 必須容忍「一個 run 對多列 DLQ 紀錄」（replay 後再失敗會累積）
- **I-14 是紅線：** 判斷 run 是否 DLQ 的唯一依據是 `runs.status`／`steps.status`，絕不是 DLQ 表裡有沒有列

### 7. Storage 啟動時 fail-fast（G-04、G-05）

- 啟動時連不上 storage ⇒ 立即以非零狀態碼退出，stderr 印出**明確指認為 storage 連線問題**的訊息
- 不做靜默重試迴圈
- 執行期斷線的錯誤日誌同樣須可辨識為 storage 問題，不得包裝成通用 step 失敗

### 8. Retry delay 的可見性（H-04d）

不開放 per-workflow 設定，但**必須讓使用者知道這段延遲存在**：

- `GET /runs/{id}` 中，處於重試冷卻期的 step 應能看出來（例如下次嘗試的預計時間，或明確的狀態標示）
- README／USER_MANUAL 說明 `X × (timeout + retry delay)` 的估算方式
- 理由：使用者會用 `X × timeout` 估算進 DLQ 的時間，實際更長

### 9. 冷卻窗口的回報處理（E-08 至 E-10）—— **這一項是查核，多半不需改 code**

TX3 之後、TX4 之前抵達的任何回報必須 200 ACK 零效果。機制上這應該**已經自然成立**（該 attempt 已非 `RUNNING`，CAS 必然不匹配）。

**你要做的是確認它靠 CAS 成立，而不是靠應用層判斷。**

**E-10 是紅線：** 若程式中存在「檢查現在是不是冷卻期」之類的分支 —— 那是第二個判斷來源，遲早與 CAS 分歧，**移除它**。同理，callback 抵達時 run 已是 DLQ 的情況也不需任何額外檢查（E-06）。

---

## 硬規則

| # | 規則 |
|---|---|
| B-1 | **TX ledger（白皮書 §19）是法律。** 每個 TXn 是單一資料庫交易，內容恰如清單所列。不得拆分、不得合併、不得把「commit 後才動作」改成「動作後才 commit」 |
| B-2 | **單一寫入者：** 只有 orchestrator loop 寫 step/run 狀態。callback handler 只做驗證、推 channel、回 200 |
| B-3 | **CAS 無所不在：** `UPDATE ... WHERE attempt_id=$X AND status='RUNNING'`。零列受影響 ⇒ 型別化的 superseded 結果，**不是 error** |
| B-4 | **時間戳一律 DB `now()`**，絕不接受 worker/planner 回報中的時間，絕不用行程時鐘排序 |
| B-5 | `test/acceptance/` 與 `spec/` **唯讀**。動了就是紅線 |
| B-6 | **驗證後才回報。** 跑完該項的驗收指令再說完成，不接受「看起來對」 |
| B-7 | 每項工作**獨立 commit**，訊息引用矩陣 ID |

---

## 完成條件

- `go build ./...`、`go vet ./...` 乾淨
- `TEST_DATABASE_URL=... go test -p 1 ./...` 全綠（既有測試不得因你的改動而失敗）
- `docker compose down -v && up -d --build` 後 `\d workflows` 顯示新欄位
- `python3 demo/crash_demo.py` 與 `./demo/run_demo.sh` scenarios 1–3 仍通過
- **兩支凍結 oracle 仍通過**（`crash_recovery_test.py`、`EXPECT_X=2 dlq_replay_test.py`）
- 貼出以下證據：`git status --porcelain test/acceptance/ spec/`（**應為空**）與 `sha256sum spec/BEHAVIOR_MATRIX.md`

**已知環境限制：** `crash_recovery_test.py` 在 Docker Desktop／WSL2 拓樸下會確定性失敗（`host.docker.internal` 轉發問題，Session 21 以 6/6 對照實驗證實與程式碼無關）。**若你遇到這個特徵，記錄下來但不要為了讓它通過而改動 code。** 已知可行的做法是把 `ADVERTISE_HOST` 設為 WSL2 介面的實際 IP。

---

## 報告格式

1. **動工前三件查核的實測結果**（放最前面）
2. 檔案變更清單，每個一行說明
3. 每項工作 → 對應的矩陣 ID → 驗收指令與 **verbatim 輸出**
4. TX 對映：本次觸及的每個 ledger TX → `file:function`，並確認各為單一 `BEGIN…COMMIT`
5. **偏離點：** 任何與本 prompt 不同的做法 —— 做了什麼、為什麼。**空白欄位可疑：真實實作幾乎不可能零偏離**
6. 你發現的矩陣／白皮書／既有程式碼之間的不一致 —— **回報，不要自行裁決**
7. 未解問題，分兩類：🟡 已停下需裁示 ／ 🔴 自行假設後繼續（每條都可能是規格偏移）
8. **反向問題（這題可能最有價值）：現有程式碼中，有哪些行為在行為矩陣裡找不到對應的列？** 這些可能是：規格漏寫的、你或前人加的未記錄判斷、或真正的死碼。**逐條貼出 `file:function` 與原始程式碼區塊，不接受摘要。**
