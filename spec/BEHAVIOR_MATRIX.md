# StateFlow 行為矩陣（BEHAVIOR_MATRIX）

**狀態：v2.0 — 純行為規格。實作指示與測試基礎設施已移出。**

---

## 0. 這份文件是什麼

**一句話：系統收到什麼、必須做出什麼可觀測的反應。**

每一列都是一條斷言：一個觸發情境，一個必須成立的結果。除此之外，這份文件不含任何東西——沒有實作步驟、沒有測試工具的規格、沒有環境資訊、沒有待辦清單、沒有專案歷史。

### 這裡不寫什麼，以及它們在哪

| 不屬於本檔的東西 | 正確位置 |
|---|---|
| 怎麼改 code、怎麼寫 migration、動工前查什麼 | 實作 session 的 prompt |
| 測試分幾層、fake 要有什麼能力、CI 怎麼接、本機怎麼跑 | 測試 session 的 prompt |
| 本機環境、平台差異、工具鏈有沒有安裝 | `docs/OPERATIONAL_FACTS.md` |
| 白皮書要補什麼、未來想做什麼 | `docs/BACKLOG.md` |
| 專案歷史、哪個 session 做了什麼 | `STATE_SNAPSHOT.md` |

**判準：如果一句話不是「系統在 X 情況下必須 Y」，它就不該在這裡。**

### 為什麼要這麼嚴

這份文件的權威性完全來自它的純粹。混進實作指示，它就變成待辦清單；混進環境資訊，它就會過期；混進測試工具規格，寫測試的人就會以為那些也是系統該有的行為。**任何一種混入都會讓「測試失敗＝實作有問題」這個推論失效。**

### 作者權

| 檔案 | 作者 | Claude Code 權限 |
|---|---|---|
| `spec/BEHAVIOR_MATRIX.md`（本檔） | 架構者，經 owner 核可 | **唯讀** |
| `spec/MATRIX_FINDINGS.md` | Claude Code | 可寫 |

findings 按情境 ID 對齊，兩份文件實體上永不共編。凍結的驗證方式定義在各 session 的 prompt 中，不在本檔。

---

## 1. 欄位定義

| 欄位 | 含義 |
|---|---|
| **ID** | 穩定識別碼，永不重編號。作廢的列標 `[已作廢]` 保留原號 |
| **觸發情境** | 發生了什麼 |
| **預期結果** | 必須成立的**可觀測**狀態。這一欄就是斷言 |
| **落點** | 事件結束後落在哪個合法組合（L1–L5，見下） |
| **來源** | `§n` = 白皮書章節；`裁定 #n` = M 節的 owner 裁定 |

### 落點代碼（白皮書 §8.2 的五種合法組合）

| 代碼 | 組合 | 重啟時的動作 |
|---|---|---|
| **L1** | run=RUNNING, last_step=DONE（或無 step） | 重新問 planner |
| **L2** | run=RUNNING, last_step=RUNNING | recovery 三步驟 |
| **L3** | run=DONE, last_step=DONE | 不碰 |
| **L4** | run=DLQ, last_step=DLQ | 不碰（worker 側） |
| **L5** | run=DLQ, last_step=DONE | 不碰（planner 側） |

---

## A. 正常路徑

| ID | 觸發情境 | 預期結果 | 落點 | 來源 |
|---|---|---|---|---|
| A-01 | `POST /workflows`，planner_type=static 或 http | TX-W 單一交易寫入 workflow 定義（含 planner_type + planner_config）；回傳 **201** 與 workflow_id | — | §19, §12.1 |
| A-02 | `POST /workflows/{id}/runs` 帶 workflow_input | TX0 建 run（RUNNING）；回傳 **202** 與 run_id；啟動**恰好一個** loop goroutine | L1 | §5, §19 |
| A-03 | loop 讀 frontier，planner 回 continue + 完整 StepSpec | TX1 單一交易：建 step（RUNNING、seq=MAX+1、attempt_count=0、decision=完整 StepSpec）＋ 建首個 attempt（RUNNING）＋ 設 current_attempt_id。**commit 之後才准派送** | L2 | §19 TX1, §2 |
| A-04 | sync worker 回 2xx + 合法 JSON body | TX2 單一交易：attempt→DONE ＋ step→DONE ＋ 寫 output（整個 body）。**commit 之後才准問下一次 planner** | L1 | §19 TX2, §13.2 |
| A-05 | StepSpec 指定 output_field 且該欄位存在 | output 只存該子樹，不是整個 body | L1 | §13.2 |
| A-06 | async worker 回 202，之後 `POST /tasks/complete` 帶正確 ids | callback handler 只驗證＋推 channel＋回 200；狀態寫入由 loop 執行 TX2 | L1 | §10.4, §19 TX2 |
| A-07 | loop 再次讀 frontier | history 為全部 DONE steps，依 `seq` 升冪，含每步完整 output | — | §12.2 |
| A-08 | planner 回 done | TX7：run→DONE。此 run 永不再被掃描、永不加標籤 | L3 | §19 TX7, §11 |
| A-09 | 同一 workflow 同時啟動多個 run | **完全支援。** run 是系統的併發單位；同 workflow 只代表共用同一份設定。各 run 有獨立 goroutine、獨立 seq 序列、獨立預算，互不影響 | L1/L2 | 裁定 #5 |
---

## B. Worker 失敗（四種 reason）

| ID | 觸發情境 | 預期結果 | 落點 | 來源 |
|---|---|---|---|---|
| B-01 | sync worker 回非 2xx | attempt→FAILED(`worker_reported`)，HTTPStatus 記錄於 error | L2 | §4.2, §13.2 |
| B-02 | async worker 回呼 `/tasks/fail` | attempt→FAILED(`worker_reported`) | L2 | §13.2 |
| B-03 | sync worker 超過 effective timeout 未回應 | attempt→FAILED(`timeout`) | L2 | §6 |
| B-04 | async worker 回 202 後靜默超過 timeout | `select(channel, timer)` 中 timer 勝出 → attempt→FAILED(`timeout`)。**不得裸等 channel** | L2 | §6 |
| B-05 | sync worker 回 2xx 但 body 非合法 JSON | attempt→FAILED(`malformed`) | L2 | §4.2, §13.2 |
| B-06 | sync worker 回 2xx，但 StepSpec 宣告的 output_field 不存在 | attempt→FAILED(`malformed`) | L2 | §13.2 |
| B-07 | async callback ids 合法但 output 不可解析 | attempt→FAILED(`malformed`) | L2 | §7.1 |
| B-08 | async callback 缺少或帶非法 step_id/attempt_id | 回 **400**，**零狀態效果**（等該 attempt 自己的 timeout 認領） | L2 | §7.1 |
| B-09 | 任一失敗且 attempt_count < X | TX3：attempt→FAILED(reason) ＋ count++；等待**生效重試延遲**（H-04）；TX4：建新 attempt ＋ CAS 換 current_attempt_id；再派送 | L2 | §7.1, §6 |
| B-10 | 失敗使 attempt_count 達到 X | **TX3 同一交易內**：attempt→FAILED ＋ count=X ＋ step→DLQ ＋ run→DLQ ＋ 插入一列 dead_letter_queue（reason=`worker_retry_exhausted`，context 含逐次 attempt 的 reason 與 error） | L4 | §19 TX3, §7.1 |
| B-11 | B-10 之後 | **不得再派送任何 attempt**。step 與 run 皆為終態，唯一出口是 replay | L4 | §7.1, §11 |
| B-12 | 四種 reason 分別發生 | 四者走**完全相同**的 TX3 路徑、同耗預算；分類只寫進 DB 供人閱讀，機器不依它分支 | L2/L4 | §4.2 |
| B-13 | planner 回的 StepSpec 中 mode 不是 sync 也不是 async | 缺 mode 已在 planner 驗收階段被擋（→ D-02b）；若仍抵達派送層，一律以 sync 處理 | L2 | §13.3 |
| B-14 | planner 回的 step name 與本 run 已存在的 step 同名 | **planner 的責任。** 在 TX1 之前的決策驗收階段擋下 → 分類為 `malformed`，耗 planner 預算（→ D-02）。**不得走到 PK 衝突**。依據：§12.2 的 history 已含每步 `name`，planner 看得到過去所有名稱，重名是它的錯 | L1 | 裁定 #1 |
| B-15 | static planner 的 YAML 步驟表本身含重名 | **不在執行期處理。** 於 `POST /workflows` 的 config 驗證階段擋下（→ N-06）。static planner 在執行期仍維持「構造上不會失敗」 | — | 裁定 #1+#B |
---

## C. Crash window（本文件的核心）

> 白皮書 §19 的驗收法：**相鄰兩次持久化寫入之間，每一個間隙都是一個 crash window。** 下表逐一認領。

| ID | 觸發情境 | 預期結果 | 落點 | 來源 |
|---|---|---|---|---|
| C-01 | planner 已回答（continue / done / fail），但 TX1/TX7/TX8 尚未 commit 就 crash | **未持久化的答案視同沒發生。** recovery 見 run=RUNNING, last_step=DONE → 重問 planner。LLM planner 這次可能給不同答案，**合法** —— 「恰問一次」只保障已持久化的決策 | L1 | §17 Q17 |
| C-02 | TX1 已 commit，但 worker 尚未被派送就 crash | 決定不遺失。recovery 認領孤兒 attempt（→FAILED(`orphaned`)＋count++），再由 TX4 **重派已存的 decision**，**不重問 planner** | L2 | §17 Q1, §8.3 |
| C-03 | worker 已被派送、正在執行或結果正在回程時 crash | 同 C-02 路徑。舊 attempt 被判 `orphaned`；worker 端可能重複執行 —— 由 worker 的 step_id 冪等吸收 | L2 | §8.3, §15.1 |
| C-04 | crash 落在 TX3 與 TX4 之間（已判失敗、新 attempt 未建） | recovery 見 step=RUNNING、last_attempt=FAILED、count<X → **沒有 RUNNING attempt 可認領**，直接走預算檢查 → TX4 建新 attempt 派送。**不得重複計入預算** | L2 | §7.1 |
| C-05 | crash 落在重試延遲當中 | 與 C-04 同一個窗口、同一條規則。**recovery 重派時完全跳過重試延遲** —— crash 本身已提供足夠冷卻 | L2 | §8.3 |
| C-06 | TX2 已 commit，但下一次 planner 呼叫前 crash | 已完成的 step **完全不被觸碰**（output 與 created_at 跨 crash 位元相同）；recovery 重問 planner | L1 | §8.2, §2 |
| C-07 | crash 發生在 recovery 執行到一半 | **recovery 可重入。** 已被認領的孤兒此時是 FAILED 不是 RUNNING，不可能被二次認領、二次計數 | L2 | §8.3 |
| C-08 | recovery 認領孤兒時 count 已是 X−1 | 孤兒認領使 count 達 X → **在 TX3 內部**直接進 DLQ；**不得派送任何新 attempt** | L4 | §8.3(b) |
| C-09 | orchestrator 反覆 crash（crash loop） | 每次 crash 的孤兒認領耗一單位預算 → 每個 in-flight step **單調收斂到 DLQ**，無限重試在結構上不可能 | L4 | §8.3 |
| C-10 | crash 恰好落在某個 TXn 的 commit 當下 | 交易語意：要嘛全發生、要嘛全沒發生。**不存在半套狀態** | 依 TX | §9, 裁定 #7 |
| C-14 | **同一個決策點永不重問**（frontier model 的核心主張） | 以 history 深度標記決策點。planner **可能**在同一深度被問多次（答案未通過驗收會重問，耗 planner 預算）。但**一旦某深度的答案被接受並持久化為一個 step，該深度永不再被詢問** —— 跨 crash、跨 recovery、跨 worker 側 replay 皆然。可觀測：對每個深度，「被接受且建立了 step 的答案」恰好一次。**兩個例外不算違反**：(a) 終局答案（done／fail）不建 step，planner 側 replay 正是撤回它，因此該深度會被重問（F-04）；(b) 呼叫總數沒有上界，被拒絕的答案耗呼叫但不建 step | — | §2, §12.1 |
| C-15 | `steps.decision` 的不可變性 | 一旦 TX1 commit，該 step 的 `decision` 與 `created_at` **永不再被寫入**（讀取兩次得到相同的值）。recovery 重派讀的是這一欄，若它會變動，「恰問一次」的保證就是空的。**注意：儲存值與 planner 送出的原始位元組不必相同**（見 K-14），此列斷言的是「存進去之後不再改變」 | L2 | §4.2, §2 |
| C-16 | 已完成 step 在 replay 之後 | 同 C-15：已 DONE 的 step 其 `output`、`created_at`、`decision` 在 worker 側 replay 前後讀取結果相同 | L2 | §11 |
| C-11 | 重啟後掃描 | 只掃 `runs.status='RUNNING'`（索引查找）。run=DONE 與 run=DLQ **永不被掃描、永不被觸碰** | — | §8.3 |
| C-12 | crash 落在「TX1 commit 之後、worker 回應之前」 | 這是 C-02／C-03 的統稱：**attempt 層級的孤兒**，DB 裡留下一列 RUNNING attempt，由 recovery 判 `orphaned` | L2 | 裁定 #7 |
| C-13 | crash 落在「已送出 planner 請求、答案未回或未持久化」 | 這是 C-01：**run 層級的孤兒，不存在孤兒 attempt**。planner 呼叫不持久化，DB 裡沒有任何東西需要認領。recovery 動作只有「重問」，**不耗 attempt 預算、不寫 failure_reason** | L1 | 裁定 #7 |
---

## D. Planner 失敗與預算

| ID | 觸發情境 | 預期結果 | 落點 | 來源 |
|---|---|---|---|---|
| D-01 | planner 超時（30s）或連線失敗 | 分類為 `unreachable`，**耗一單位 planner 預算**（總共 3 次）。這不是 attempt 失敗，不碰 attempt_count | L1 | §7.2 |
| D-02 | planner 回應不合**語法**驗收：非合法 JSON／缺 status／continue 但缺 worker_url 或 mode／JSON 外夾散文或 markdown 圍欄 | 分類為 `malformed`，耗一單位 planner 預算 | L1 | §7.2, §12.3 |
| D-02b | planner 回應語法合格但不合**語意**驗收：step name 與本 run 已有的重名（B-14）／mode 不是 sync 或 async／worker_url 不是合法的 http(s) URL | 同樣分類為 `malformed`，耗預算。**兩層驗收都在 TX1 之前完成**，不合格的決策永不落盤 | L1 | 裁定 #1,#3,#4 |
| D-03 | planner 預算 3 次耗盡 | TX9：run→DLQ ＋ DLQ 紀錄，**reason = 最後一次失敗的類別**（`planner_unreachable` 或 `planner_malformed`），全部嘗試明細寫入 context | L5 | §19 TX9, §7.2 |
| D-04 | planner 明確回 `fail` | **這是合法答案，不是失敗。** TX8：run→DLQ ＋ DLQ 紀錄（`planner_declared_fail`）。**不耗預算** | L5 | §19 TX8, §7.2 |
| D-05 | planner 預算計數期間 orchestrator crash | 計數在記憶體，歸零。**安全，因為 planner 呼叫無副作用且在 Barrier 1 之前** | L1 | §6, §18 #3 |
| D-06 | workflow 使用 static planner | 構造上不會失敗，**跳過整個預算路徑** | L1 | §12.1 |
| D-07 | 每次 run 進入或重進 loop | planner 實例**每次從 workflow 那一列重建**（planner_type + planner_config），絕不從行程全域狀態取得。static 與 http 對 loop 不可分辨 | — | §12.1 |
| D-08 | planner 回 continue，worker_url **語法不合法**（非 http/https、空字串、無法解析成 URL） | **planner 錯誤** → `malformed`，耗 planner 預算，在 TX1 之前擋下 | L1 | 裁定 #4 |
| D-09 | planner 回 continue，worker_url 語法合法但**執行期連不上**（DNS 失敗、連線被拒、主機不存在） | **worker 失敗**，非 planner 失敗 → attempt→FAILED(`timeout`)，耗 attempt 預算。理由：這與 worker 掛掉不可分辨，必須同路 | L2 | 裁定 #4 |
---

## E. 回報處理與 CAS

| ID | 觸發情境 | 預期結果 | 落點 | 來源 |
|---|---|---|---|---|
| E-01 | 回報帶的 attempt_id ≠ steps.current_attempt_id | **200 ACK，零狀態效果**（superseded，不是 error） | 不變 | §10.1–2 |
| E-02 | 同一 attempt 的回報重複送達兩次 | 第一次生效；第二次因 attempt 已非 RUNNING 而 CAS 失敗 → 200 零效果 | 不變 | §10.1 |
| E-03 | 成功回報在該 attempt 已被判 FAILED(timeout) **之後**才抵達 | **一律拒絕**（MVP）。不允許 failed→done 復活轉移。工作重跑一次是可接受代價 | 不變 | §10.3, §18 #2 |
| E-04 | 任何 callback 抵達 | callback handler 只做：驗證 → 推 channel → 回 200。**永不寫 step/run 狀態**（單一寫入者原則；第二寫入者會與 timer 競爭終局） | — | §10.4 |
| E-05 | timeout timer 觸發後，遲到的 callback 才抵達 | timer 觸發時必須**清乾淨 channel registry**，使遲到 callback 找不到登記項而被 ACK 為 superseded | 不變 | §10.2 |
| E-06 | callback 抵達時該 run 已經是 DLQ | **不需任何額外檢查**：該 attempt 早已非 RUNNING，CAS 自然不匹配 → 200 零效果。這是刻意把判斷交給 DB 層的設計利用，應用層不得重複實作這個檢查 | 不變 | 裁定 #6 |
| E-08 | **重試延遲窗口內抵達的任何回報** | 一律 200 ACK、**零狀態效果**。這是 E-03 的一般化：不只「超時後的成功」被拒，**冷卻期間的任何回報一律不承認、不採用、不寫入**。**由哪一層攔下不構成規格** —— 可能是 CAS，也可能更早在 transport 層就沒有登記項 | 不變 | 裁定 #G |
| E-09 | 上述窗口的邊界 | 窗口自 TX3 commit 起、至 TX4 commit 止。TX4 commit 之後抵達的是**新 attempt** 的回報，帶舊 attempt_id 者仍被 CAS 擋掉（E-01） | 不變 | 裁定 #G |
| E-10 | 冷卻窗口內的回報 vs 其他 superseded 回報 | 兩者**行為完全相同**：同樣 200、同樣零效果、同樣不產生任何狀態差異。**本條只約束回報處理路徑** —— 唯讀查詢揭露冷卻狀態（F-06e 的 `retry_state`／`next_attempt_not_before`）不受此限，它不 gate 任何回報 | 不變 | 裁定 #G+#6 |
| E-07 | 回報遇到非 current 或非 RUNNING 的 attempt | 一律回傳**型別化的 superseded 結果，不是 error**。呼叫端據此回 200、零效果。判斷必須同時涵蓋兩個條件：該 attempt 仍為 RUNNING，**且**它仍是該 step 的 current_attempt_id —— 少了後者 E-01 不成立 | — | §19 CAS-A |
| E-11 | 回報帶的 attempt_id 格式正確、真實存在，但屬於**別的 step** | **200 ACK，零狀態效果**，與其他 superseded 情況不可分辨。不得為了區分「屬於別的 step」而多做一次查詢 —— 那是第二個判斷來源。B-08 的 400 只適用於畸形 ids（缺漏或無法解析） | 不變 | 裁定 #J |
---

## F. DLQ 與 Replay

| ID | 觸發情境 | 預期結果 | 落點 | 來源 |
|---|---|---|---|---|
| F-01 | worker 側耗盡進 DLQ | 恰好一列 dead_letter_queue，reason=`worker_retry_exhausted`，step_id 非 null，context **非空**且含逐次 attempt 的 reason 與 error | L4 | §11, §7.1 |
| F-02 | planner 側進 DLQ | dead_letter_queue 的 step_id 為 **null**（無 step 有過錯），reason 為三種 planner 值之一 | L5 | §14.1, §11 |
| F-03 | `POST /dlq/{id}/replay`，該 run 目前為 **worker 側 DLQ**（`run=DLQ ∧ last_step=DLQ`） | TX5 單一交易：**attempt_count 歸零** ＋ step→RUNNING ＋ run→RUNNING ＋ 建新 attempt ＋ 設 current_attempt_id → 派送。**歸零是強制的** | L2 | §19 TX5, §11 |
| F-04 | `POST /dlq/{id}/replay`，該 run 目前為 **planner 側 DLQ**（`run=DLQ ∧ last_step=DONE` 或無 step） | TX6：run→RUNNING → 重問 planner | L1 | §19 TX6 |
| F-04b | 分支的判斷依據 | **一律由 run 與 last_step 的現況推導（組合表），不看 entry 的 `step_id`。** 多輪 run 用舊 entry id replay 時兩者會分歧；現況是真相，entry 是歷史（I-14） | — | 裁定 #K |
| F-05 | replay 之後 | 已 DONE 的 steps **不得重跑**；planner 收到的 history 仍完整 | L1/L2 | §2 |
| F-06 | `GET /runs/{id}` 的頂層形狀 | 恆有 `run_id`、`status`、`workflow_input`、`created_at`、`updated_at`、`steps`（陣列）。`dlq_reason` **僅在 `status="DLQ"` 時出現** | — | §11 |
| F-06b | `steps` 陣列中每一項的形狀 | 恆有 `step_id`、`step_name`、`seq`、`status`、`attempt_count`、`created_at`。**注意 `step_name`：與送給 planner 的 history 中的 `name` 是同一個東西，但兩個表面用不同鍵名**（J-04 是 planner 契約，此處是讀取 API） | — | §11 |
| F-06c | 選填欄位的表示法 | **系統產生的**選填欄位（`output`、`completed_at`、`current_attempt`、`reason`、`error`、`retry_state`、`next_attempt_not_before`、DLQ entry 的 `step_id`）在無值時**整個鍵不出現**，不是 `null`。**斷言必須檢查鍵是否存在，不得比對 `== null`**。**此規則不適用於使用者資料**：`workflow_input` 與 step 的 `output` 內部可合法含 `null`，其內容一律原樣透傳，不得清理 | — | §11 |
| F-06d | `current_attempt` 的內容 | **單數，只含最新的一個 attempt**，恆有 `attempt_id` 與 `status`；失敗時另有 `reason` 與 `error`。**完整的 attempt 歷史不在此處**，只在 DLQ entry 的 `context.attempts` | — | §11 |
| F-06e | 重試冷卻期的可見性（承 H-04d） | 處於冷卻期的 step 帶 `retry_state` 與 `next_attempt_not_before`；非冷卻期時兩鍵不出現 | — | 裁定 #G |
| F-07 | `GET /runs/{id}` 的欄位命名 | step 時間欄位名為 `created_at`。JSON 中**任何地方都不得出現 `decided_at`** | — | §14.1 |
| F-08 | 對同一個 DLQ entry 連續呼叫 replay 兩次 | **第一次**的 TX5／TX6 在單一交易內把 run 帶離 DLQ（→RUNNING），這本身就是冪等閘門。**第二次**檢查 run 現況：非 DLQ ⇒ 回 **409 Conflict**，訊息明說「此 run 已在執行中」，零狀態效果。判斷依據是 **run 的現況，不是 DLQ entry 是否存在** | 不變 | 裁定 #2 |
| F-09 | 四種 DLQ reason | 純資訊性，**replay 機制完全相同**；差別只在人的分診動作。`planner_declared_fail` 不可盲目 replay | — | §11 |
| F-10 | `GET /dlq` 的形狀與預設過濾 | 回應為 `{"entries": [...]}`。預設**只列出目前 `run_status="DLQ"` 的項目**；`?all=true` 列出全部（含已被 replay 帶離 DLQ 的歷史） | — | 裁定 #2 |
| F-10b | 每一筆 entry 的形狀 | 恆有 `id`、`run_id`、`reason`、`context`、`created_at`、`run_status`。`step_id` **僅 worker 側有**（planner 側整個鍵不出現）。`side` 僅在 `run_status="DLQ"` 時出現（組合表不分類已離開 DLQ 的 run） | — | 裁定 #8 |
| F-10c | `context` 的形狀 | **兩側必須都是真正的 JSON 物件，不得雙重編碼。** 恆有 `detail`（最後一次失敗的錯誤字串）與 `attempts`（陣列，逐次嘗試的明細）。worker 側另有 `step_id` | — | 裁定 #I |
| F-10d | `side_conflict` | 由組合表推導出的 side 與 entry 的 `step_id` 不一致時出現，明示衝突而非靜默擇一 | — | 裁定 #8 |
| F-11 | `GET /dlq` 每一項的分類 | 由組合表推導，**不新增儲存欄位**：run=DLQ ∧ last_step=DLQ ⇒ **worker 側**；run=DLQ ∧ last_step=DONE（含無 step）⇒ **planner 側**。前者 step_id 非 null，後者為 null，兩者必須一致 | L4/L5 | 裁定 #8, §8.2 |
| F-12 | replay 後再次失敗 | `dead_letter_queue` 會**累積第二列**（歷史紀錄永不刪除）。同一 run 可有多列 DLQ 紀錄，時間戳區分輪次。`GET /dlq` 與 replay 的查找必須容忍「一個 run 對多列」 | L4/L5 | 裁定 #2, §11 |
| F-13 | 對「run 目前不是 DLQ」的 entry 呼叫 replay | 同 F-08：409，且訊息須指出目前的實際狀態（RUNNING／DONE），讓操作者知道發生了什麼 | 不變 | 裁定 #2 |
---

## G. Storage 故障

| ID | 觸發情境 | 預期結果 | 落點 | 來源 |
|---|---|---|---|---|
| G-01 | Postgres 掛掉 | API 拒絕新請求；每個 in-flight run 的 goroutine 在第一次寫入失敗時死亡，run 停在 RUNNING —— **孤兒但完整** | L2 | §9 |
| G-02 | storage 恢復 ＋ orchestrator 重啟 | recovery 用**與任何 crash 完全相同的路徑**撿回這些 run | L2 | §9 |
| G-03 | storage 故障期間的資料完整性 | **零損失、無半套狀態**。所有寫入皆為原子交易，已持久化狀態任何時刻完整一致 | — | §9 |
| G-04 | storage 未恢復但 orchestrator 重啟 | **Fail fast。** 啟動時若無法連上 storage，orchestrator 立即以非零狀態碼退出，並在 stderr 印出**明確指出是 storage 連線問題**的訊息（不得只印通用錯誤、不得靜默重試迴圈）。整個 orchestrator 建立在「storage 完好」的假設上，這個假設不成立時唯一正確的行為是大聲地停下來 | — | 裁定 #3, §9 |
| G-05 | 執行期 storage 斷線（非啟動時） | goroutine 於第一次寫入失敗時死亡（G-01）。錯誤日誌同樣必須可辨識為 storage 問題，而非被包裝成通用的 step 失敗 | L2 | 裁定 #3 |
| G-06 | storage 短暫故障後恢復，orchestrator **未重啟** | 行程內 sweeper 於下一次週期（預設 30 秒）認領該孤兒 run 並續跑，**不需要人為重啟** | L2 | §18 #4 |
| G-07 | sweeper 的認領路徑 | 與 crash recovery **完全相同**的路徑（認領孤兒 attempt → 預算檢查 → 重派或已進 DLQ）。因此 C-07 的可重入性與 C-09 的收斂性原封不動適用 | L2 | §18 #4 |
| G-08 | sweeper 掃到一個已由其他路徑接手的 run | 不得二次認領、不得二次計入預算。判斷依據與 recovery 相同（該 attempt 是否仍為 RUNNING） | L2 | C-07 |
---

## H. Timeout 解析與時間規則

| ID | 觸發情境 | 預期結果 | 落點 | 來源 |
|---|---|---|---|---|
| H-01 | StepSpec 有 timeout_seconds | 生效優先序：**step 覆寫 > workflow 預設 > 系統預設 60s**。timeout_seconds=0 表示向上繼承 | — | §6 |
| H-02 | attempt 的計時起點 | **錨定在 attempt 建立時刻，不是派送時刻。** deadline = attempt created_at + effective timeout，由 loop 算好用 `context.WithDeadline` 傳入 transport；transport 不得自起時鐘 | — | §6 |
| H-03 | TX1 commit 後、派送前卡住 | 由**同一支 timer** 認領 →`timeout`。「有決定、無結果」整段期間恰好被一條規則涵蓋 | L2 | §6 |
| H-04 | 重試延遲的來源與可設定性 | **系統預設 5 秒，可由環境變數 `RETRY_DELAY_SECONDS` 覆寫**。格式錯誤時 fail-fast，不靜默退回預設。與 timeout 是**兩個不同旋鈕**，不可混淆 | — | §18 #6 |
| H-04b | worker 回報 `retry_after_seconds` 時的生效延遲 | **取大值：`max(retry_after_seconds, 系統預設)`**。worker 說「等更久」會被採納，說「等更短」不會 —— 系統預設是地板 | — | §18 #5 |
| H-04c | `retry_after_seconds` 為 0 或負數 | 取大值規則自然使其退回系統預設。**不得因此縮短延遲** | — | §18 #5 |
| H-04d | 重試延遲對使用者的可見性 | `GET /runs/{id}` 必須讓處於重試冷卻期的 step 可被辨識（例如下次嘗試的預計時間，或明確的狀態標示） | — | 裁定 #G |
| H-05 | 所有時間戳的來源 | 一律為交易 commit 當下的 DB `now()`。**絕不採用 worker/planner 回報內容中的時間，絕不用行程時鐘排序** | — | §3 |
| H-06 | attempt 的排序依據 | `created_at`。**不存在 attempt_number 欄位** | — | §4.2 |
| H-07 | step 的排序依據 | `seq`，且**只有** seq。不得用時間戳或 step_id 排序 | — | §4.2 |
| H-08 | 三個數字的職責分界（**最常見的理解錯誤**） | **timeout** = 一個 attempt 的生命上限，等多久之後宣判它失敗；**retry delay** = 宣判失敗後、建立下一個 attempt 前的冷卻（預設 5s，可由環境變數覆寫，且可被 worker 的 `retry_after_seconds` 拉高——見 H-04／H-04b）；**retry limit X** = 累積幾次失敗之後進 DLQ。三者互不替代 | — | §6 |
| H-09 | 一次完整重試循環的時序 | attempt 於 T 建立 → 最遲於 T+timeout 被宣判失敗（可能更早，例如 worker 直接回 500）→ 於 T+timeout+delay 建立下一個 attempt。**最壞情況總耗時 ≈ X × (timeout + delay)** | L2 | §6 |
---

## N. Config 格式與提交時驗證

> **本節在白皮書中沒有對應章節，是新增的規格領域。** 治理原則（owner 裁定）：**寧可過度嚴謹，讓使用者來抱怨我們不支援某件事；也不要過度寬鬆，讓他安靜地出錯。** 所有可在提交時發現的錯誤，都不准留到執行期。

### N.1 格式一致性

| ID | 觸發情境 | 預期結果 | 來源 |
|---|---|---|---|---|
| N-01 | 使用者提供的設定檔 | **副檔名 `.yaml` 的檔案，其內容必須能被 YAML 解析器解析為預期結構** | 裁定 #A |
| N-02 | 檔案格式 vs 線上格式的分界 | **檔案端一律 YAML**（可寫註解，對 demo 與 portfolio 價值高）；**HTTP API 端一律 JSON**（`POST /workflows` 的 body）。DB 端 `planner_config` 仍為 JSONB，不需 schema 變更 | 裁定 #A |
| N-03 | 混用 | 不存在副檔名與內容格式不符的設定檔 | 裁定 #A |
### N.2 提交時驗證（`POST /workflows`）

> **治理原則（owner 裁定 #3）：輸入契約與儲存形狀是兩件事，必須分開。** API 接收的 body 有它自己的 schema，驗證只針對這份 schema；DB 的 JSONB 只是落地形狀，由 API 層正規化後寫入。**驗證器不得把 DB 的欄位形狀當成輸入契約**——那會讓儲存細節洩漏到使用者介面，並使日後改 schema 變成破壞性變更。

| ID | 觸發情境 | 預期結果 | 來源 |
|---|---|---|---|---|
| N-04 | `planner_type` 不是 `static` 也不是 `http` | 400，訊息指出合法值 | 裁定 #B |
| N-05 | `planner_type=http` 但 `planner_config` 缺少 URL 欄位；或 `planner_type=static` 但缺少步驟表 | 400，訊息指出缺少哪個欄位 | 裁定 #B |
| N-06 | **交叉污染**：static planner 的 config 出現 http 專屬欄位（如 URL），或 http planner 的 config 出現 static 專屬欄位（如步驟表） | 400，訊息明說「此欄位不屬於 planner_type=X」。**這是本節最重要的一條**——這類錯誤在寬鬆實作下會被靜默忽略，使用者以為設定生效了 | 裁定 #B |
| N-07 | `planner_config` 出現任何未知欄位 | 400（嚴格模式，不接受未知鍵）。合法鍵集合由**輸入 schema** 定義，與 DB 落地形狀無關 | 裁定 #B+#3 |
| N-08 | static 步驟表中兩個 step 同名 | 400（承 B-15）。static planner 在執行期的「構造上不會失敗」保證，正是靠這一條在提交時成立 | 裁定 #1+#B |
| N-09 | static 步驟表中某步驟缺 `name`／`worker_url`／`mode`，或 `mode` 不是 sync\|async，或 worker_url 語法不合法 | 400，訊息指出是第幾個步驟的哪個欄位 | 裁定 #B+#4 |
| N-09b | static 步驟表中單一 step 物件的合法鍵 | `name`、`worker_url`、`mode`、`timeout_seconds`、`retry_limit`。**其餘一律 400。** static planner 目前不支援 per-step 的 `input` 與 `output_field`；每個 worker 收到的都是 J-11 的固定形狀，前一步的 output 由 worker 自 `history` 取用 | 裁定 #L |
| N-10 | `retry_limit` 或 `default_timeout_seconds` **有帶但不合法**（非整數、`retry_limit < 1`、`default_timeout_seconds <= 0`） | 400，訊息指出欄位與合法範圍 | 裁定 #B |
| N-10b | `retry_limit` 或 `default_timeout_seconds` **未提供** | **合法**，採用預設（`retry_limit=3`、`default_timeout_seconds=60`）。與 N-07／N-18 的嚴格模式不衝突：**嚴格模式拒絕的是多餘的欄位，不是缺席的選填欄位** | 裁定 #B |
| N-11 | `retry_limit` 與 `default_timeout_seconds` 的位置 | **兩者皆為 `workflows` 表的一級欄位，且在輸入 body 中為頂層欄位。** 它們是 workflow 層級的執行參數，與「怎麼決定下一步」無關；塞進 `planner_config` JSONB 純粹是遷就舊 schema。**本輪直接改 schema，不做 API 層搬運。**詳見 N.4 | 裁定 #3+#D |
| N-12 | 驗證失敗時的副作用 | **零副作用**：不建立 workflow、不寫任何 DB 列。驗證全部先於 TX-W | 裁定 #B |
| N-13 | 驗證成功 | 才執行 TX-W，回傳 workflow_id | §19 |
| N-16 | *[已作廢]* | 舊 oracle 相容性條款；oracle 已封存 | — |
| N-17 | `POST /workflows` 帶已存在的 `name` | **允許，正常建立。** `name` 僅為顯示標籤，**不加 UNIQUE 約束**；唯一識別一律靠 `workflow_id`。理由：workflow 是可重複建立的模板，調參時會自然產生同名的多個版本，強制唯一只會逼使用者發明無意義的名字 | 裁定 #E |
| N-18 | 頂層出現未知欄位（非 `name`／`planner_type`／`retry_limit`／`default_timeout_seconds`／`planner_config`） | 400，訊息指出是哪個欄位。**與 N-07 是兩個不同層級的檢查**：一個查頂層、一個查 `planner_config` 內部，兩者互不涵蓋 | 裁定 #E |
| N-19 | 型別錯誤（`retry_limit` 給字串、`planner_config` 給陣列、`planner_type` 給數字） | 400，訊息指出欄位與期望型別。不得靜默轉型（例如把 `"2"` 當成 2） | 裁定 #E+#B |
### N.3 Planner 對自身契約的認知

| ID | 觸發情境 | 預期結果 | 來源 |
|---|---|---|---|---|
| N-14 | *[已作廢]* | 移至 `docs/BACKLOG.md` §1 #14（planner 格式規格是交付物，非系統行為） | — |
| N-15 | planner 輸出不符該契約 | 一律歸為 planner 錯誤（`malformed`），耗預算，**絕不嘗試「猜測修正」**（不剝 markdown 圍欄、不容錯解析、不補預設值）。容錯解析會讓錯誤靜默通過，違反本節治理原則 | 裁定 #C |
---

### N.4 Schema 的可觀測要求

> 本節描述 schema **必須呈現的樣子**，不描述怎麼改到那個樣子。

| ID | 要求 | 來源 |
|---|---|---|
| N-20 | `workflows` 表**必須有** `retry_limit` 與 `default_timeout_seconds` 兩個一級欄位，各帶值域約束（`retry_limit >= 1`、`default_timeout_seconds > 0`）。理由：兩者皆非 planner 設定，且藏在 JSONB 裡使 schema 無法呈現系統有這兩個旋鈕 | 裁定 #D |
| N-21 | 這兩個值**不另開表**。它們與 workflow 一對一、同時建立、同時讀取 | 裁定 #D |
| N-22 | `planner_config` **只含該 planner type 專屬的設定**：`http` ⇒ `{url}`；`static` ⇒ `{steps}`。其他任何鍵皆不合法 | 裁定 #D |
| N-23 | timeout 生效優先序：step（StepSpec）> workflow（`default_timeout_seconds`）> 系統預設 60s | §6 |
| N-24 | X 生效優先序：step（StepSpec.retry_limit）> workflow（`workflows.retry_limit`）。**X 不得來自 `planner_config`** | 裁定 #D+#F |
| N-25 | per-step 的 X 存於 `steps.decision` 之內，**不新增 DB 欄位**。判定當下用的是落盤時的值 | 裁定 #F |
| N-26 | StepSpec 的 `retry_limit` 缺欄位或 0 ⇒ 繼承 workflow；負數或非整數 ⇒ 語意驗收失敗（`malformed`），不落盤 | 裁定 #F |
| N-27 | static 步驟表中的 per-step `retry_limit` 同樣支援；值域錯誤在提交時擋下 | 裁定 #F |
| N-28 | timeout 與 retry limit **皆為兩層覆寫**，對稱 | 裁定 #F |

---

## I. 不變量（可直接寫成 SQL 斷言）

> 這一節與其他節不同：它不是情境，是**任何時刻都必須成立的條件**。適合寫成一個掃描全庫的健檢測試。

| ID | 不變量 | 來源 |
|---|---|---|---|
| I-01 | last_attempt=DONE ⇒ step=DONE（TX2 同刀） | §8.1 |
| I-02 | attempt_count=X ⇒ step=DLQ ∧ run=DLQ（TX3 同刀） | §8.1 |
| I-03 | run=DONE ⇒ last_step=DONE | §8.1 |
| I-04 | last_step=DLQ ⇒ run=DLQ | §8.1 |
| I-05 | 不存在 status=FAILED 而 failure_reason 為 NULL 的 attempt（DB CHECK 亦應擋下） | §14.1 |
| I-06 | step.status='DONE' ⇔ output IS NOT NULL。**若兩者分歧（實作缺陷），以 output 為準** | §4.1, §19 |
| I-07 | **不可能組合**：run=DONE 而 last_step 為 RUNNING 或 DLQ | §8.2 |
| I-08 | **不可能組合**：run=RUNNING 而 last_step=DLQ | §8.2 |
| I-09 | 同一 run 至多一個 status=RUNNING 的 step（序列 loop 不變量） | §5 |
| I-10 | 每個完成的 step 至多一個 status=DONE 的 attempt（無重複 checkpoint） | §2 |
| I-11 | attempt_count 只由 TX3 遞增、只由 TX5 歸零，其他任何地方不得寫 | §4.1 |
| I-12 | 資料庫中不存在 DECIDED、step 層的 FAILED、attempt_number、decided_at、dispatched_at、replay_round | §4.2 |
| I-13 | **現況表唯一、歷史表可累積。** `runs` 每個 run 恰好一列（run_id 為 PK）、`steps` 每個 step 恰好一列——它們記錄的是**當下狀態**。`dead_letter_queue` 同一個 run 可有多列——它記錄的是**歷史事件**。兩者職責不得混淆 | 裁定 #4, §14.1 |
| I-14 | 「這個 run 現在是不是 DLQ」的唯一判斷依據是 `runs.status` 與 `steps.status`，**絕不是 `dead_letter_queue` 裡有沒有列**。任何讀取路徑若只查 DLQ 表就下結論，即為缺陷 | 裁定 #4 |
---

## J. 線上格式（wire format，位元級契約）

| ID | 觸發情境 | 預期結果 | 來源 |
|---|---|---|---|---|
| J-01 | 派送 sync worker | POST body 就是 planner 決定的 input **本身**：無信封、無新增欄位、無移除欄位、值不變。識別碼一律走 header，不進 body。**保證的是 JSON 文件等價，不是位元組等價**（見 K-14） | §13.1 |
| J-11 | static planner 決定的 input 形狀 | worker 收到的裸 body 為 `{"workflow_input": <本 run 的輸入>, "history": <至今的步驟歷史>}`。**這是 static workflow 的 worker 作者必須遵守的線上契約** | §12.1 |
| J-12 | J-11 的 history 與上界化的交互 | 該 history 是**已依 J-08／J-09 上界化**的版本。因此 worker 可能收到指標物件而非前一步的完整 output。**worker 作者必須預期這件事** | §18 #1 |
| J-02 | 派送 sync worker | 必帶 header `X-StateFlow-Step-ID` 與 `X-StateFlow-Attempt-ID`，值正確 | §13.1 |
| J-03 | 派送 async worker | POST body 是信封 `{step_id, attempt_id, input}` | §13.1 |
| J-04 | 送給 planner 的 RunState | history 中**每一個 status 字串皆為大寫**（"DONE"），與儲存值一致 | §12.2 |
| J-05 | planner 回的 StepDecision | 其 `status` 為**小寫**（continue/done/fail）。J-04 與 J-05 是兩個不同欄位、兩套不同大小寫規則，| §12.3 |
| J-06 | 送給 planner 的 history 順序 | 依 seq 升冪 | §12.2 |
| J-08 | 送給 planner 的 history 中，某一步的 output 超過 **2 KB**（marshaled） | 該筆的 `Output` 被替換為小型指標物件，**不送完整內容**。完整值仍可由 `GET /runs/{run_id}` 取得。持久化的內容不受影響 | §18 #1 |
| J-09 | 保留的 Output 累計超過 **50 KB** | 自**最新往回走**分配額度，超出額度的較早項目整筆丟棄 `Output`（保留 name 與 status）。**走訪方向寫反會靜默反轉整個功能語意** | §18 #1 |
| J-10 | 50 KB 上限的計算範圍 | **只計 Output 位元組**，不含 name／status／結構開銷。因此實際 payload 會略大於 50 KB | §18 #1 |
| J-07 | `/tasks/fail` 帶 `retry_after_seconds` | **可選，接受，且已生效**：作為重試延遲的下限，見 H-04b。 | §18 #5 |
---

## K. 明確**不**保證的事（護欄區）

> **用法：往上面任何一節新增預期行為之前，先查這一節。** 命中的話它不是缺陷，是刻意的設計邊界，**不得寫成失敗測試**。

| ID | 不保證的事 | 實際承諾 | 來源 |
|---|---|---|---|
| K-01 | **step 內部的 exactly-once** | 只保證 step 間 exactly-once（完成的 step 永不重跑）。同一 step_id 最壞情況**同時有 X 路併發重複呼叫**（timeout 誤殺可能與仍活著的 worker 賽跑，不只 crash 重派）。去重是 worker 責任，建議以 step_id 為鍵 | §15.1 |
| K-03 | 遲到的成功結果會被回收 | 超時後抵達的成功一律丟棄，工作重跑 | §10.3 |
| K-04 | planner 重試計數跨 crash 保留 | 記憶體計數，crash 歸零。因 planner 呼叫無副作用故安全 | §6 |
| K-10 | 多副本 | **僅支援單一 replica** —— 多個 orchestrator 的 recovery 掃描與 sweeper 會重複認領。sweeper 使此限制更關鍵：衝突窗口從「同時重啟」變成「每 30 秒一次」 | §21 |
| K-11 | 認證授權 | 完全沒有。生產環境須置於 gateway/mesh 之後 | §15.5 |
| K-12 | **單一 run 內的 fan-out** | 一 run 一 goroutine、step 嚴格序列。planner 一次只能決定一個 step。**多個 run 併發是完全支援的**（A-09） | §5, 裁定 #5 |
| K-13 | **具名 worker 註冊表** | planner 直接給完整 `worker_url`，系統沒有已知 worker 的清單，因此不存在「查無此 worker」。語法檢查歸 planner（D-08），連不上歸 worker（D-09） | 裁定 #4 |
| K-14 | **body 的位元組穩定性** | 不保證。`decision` 存為 `jsonb`，鍵順序與空白在寫入時被正規化，因此 recovery 重派時 worker 收到的 bytes 可能與首次派送不同 —— **同一份 JSON 文件，不同的位元組**。冪等**不得**以 raw body 的雜湊為鍵；請用 `X-StateFlow-Step-ID`（J-02 保證它每次派送都在） | 裁定 #H |
| K-15 | **`GET /runs/{id}` 不提供完整的 attempt 歷史** | 只給 `current_attempt`（最新一個）。完整歷史僅在該 step 進 DLQ 之後，於 DLQ entry 的 `context.attempts` 中可見。這是刻意的：`GET /runs` 是狀態查詢，不是稽核介面 | 裁定 #I |
---

## O. 執行期組態與可觀測端點

| ID | 觸發情境 | 預期結果 | 落點 | 來源 |
|---|---|---|---|---|
| O-01 | orchestrator 啟動，三個整數環境變數皆未設定 | 生效值為 `RETRY_MAX_ATTEMPTS=3`、`RETRY_DELAY_SECONDS=5`、`SWEEP_INTERVAL_SECONDS=30` | — | §18 #6 |
| O-02 | 任一整數環境變數被設為非正整數（非數字、`0`、負數） | **fail-fast**：以非零狀態碼退出，錯誤訊息指出是哪一個變數與它的值。**絕不靜默退回預設** | — | §18 #6 |
| O-03 | 三個整數環境變數被設為合法正整數 | 生效值等於所設之值，且該值可由啟動日誌觀察到 | — | §18 #6 |
| O-04 | `DATABASE_URL` 未設定或為空 | fail-fast，錯誤訊息明確指出是 storage 設定問題（同 G-04） | — | 裁定 #3 |
| O-05 | `GET /healthz`，storage 可連線 | 200。**純讀，不得寫入任何狀態** | — | §18 #8 |
| O-06 | `GET /healthz`，storage 不可連線 | 503 | — | §18 #8 |
| O-07 | `stateflow healthcheck` CLI 子指令 | 等同 `GET /healthz`：健康離場碼 0，不健康離場碼 1。**必須在無 shell 的 distroless 環境可用** | — | §18 #8 |
| O-08 | `GET /ui` 的回應 | 200，回傳自足的 HTML，無外部 CDN 依賴 | — | §18 #8 |
| O-08b | 頁面載入與重新整理時發出的請求 | **只有讀取請求**（`GET /runs/{id}`、`GET /dlq`）。不得有任何自動發出的狀態變更請求 | — | 裁定 #M |
| O-08c | 使用者明確操作觸發的請求 | 允許狀態變更（例如點擊 replay 送出 `POST /dlq/{id}/replay`）。**必須由明確的使用者動作觸發**，不得為自動、輪詢或副作用式 | — | 裁定 #M |
| O-10 | orchestrator 啟動，資料庫尚未套用最新 migration | 自動套用所有未套用的 migration，**且在開始服務 HTTP 之前完成**。全新的空資料庫啟動後即具備完整 schema，不需任何手動步驟 | — | §18 #7 |
| O-11 | 啟動順序 | migration 套用 → recovery 掃描 → 開始服務。三者的先後可由啟動日誌觀察 | — | §18 #7 |
| O-09 | `GET /ui` 讀取的欄位 | 必須全部存在於 `GET /runs/{id}` 與 `GET /dlq` 的實際回應中（F-06、F-10 定義的形狀）。頁面上不得出現 `undefined` | — | F-06 |

---

## L. 完整性自檢清單

> 純機械核對，不需要理解設計。用來檢查「有沒有漏掉一整個維度」。

| # | 檢查 | 判準 |
|---|---|---|
| L-1 | **TX ledger 覆蓋** | §19 的 13 個項目各至少出現在一列：TX-W→A-01；TX0→A-02；TX1→A-03；TX2→A-04；TX3→B-09/B-10；TX4→B-09；TX5→F-03；TX6→F-04；TX7→A-08；TX8→D-04；TX9→D-03；CAS-A→E-07 |
| L-2 | **attempt failure reason 覆蓋** | 四值各至少一列：worker_reported→B-01/B-02；timeout→B-03/B-04；malformed→B-05/B-06/B-07；orphaned→C-02 |
| L-3 | **DLQ reason 覆蓋** | 四值各至少一列：worker_retry_exhausted→B-10；planner_unreachable／planner_malformed→D-03；planner_declared_fail→D-04 |
| L-4 | **合法組合覆蓋** | L1–L5 每一個都至少出現在一列的「落點」 |
| L-5 | **不可能組合覆蓋** | §8.2 的兩個不可能組合各有一列不變量（I-07、I-08） |
| L-6 | **crash window 覆蓋** | 每一對相鄰的持久化寫入都有一列 C-：run 建立↔TX1（C-01）、TX1↔派送（C-02）、派送↔回報（C-03）、TX3↔TX4（C-04/C-05）、TX2↔下次 planner（C-06） |
| L-7 | **API endpoint 覆蓋** | 九個路由各至少一列：POST /workflows→A-01；POST /workflows/{id}/runs→A-02；GET /runs/{id}→F-06；POST /tasks/complete→A-06；POST /tasks/fail→B-02；GET /dlq→F-10；POST /dlq/{id}/replay→F-03/F-04；GET /healthz→O-05/O-06；GET /ui→O-08 |
| L-8 | **核心主張有直接斷言** | 「同一個決策點永不重問」必須有可觀測的驗證方式（C-14），而非只由其他規則間接推導 |
| L-9 | **純度檢查** | 隨機抽十列，每一列都必須能改寫成「系統在 X 情況下必須 Y」。改寫不了的（實作步驟、環境資訊、待辦、工具規格）就是雜質，移出 |
| L-10 | **來源可追溯** | 每一列的來源欄指向白皮書章節或 M 節的裁定編號。**不得引用 session 編號或快照** —— 那是專案歷史，不是規格依據 |
| L-11 | **規格與白皮書一致** | 本檔有而白皮書沒有的規格，必須列在 `docs/BACKLOG.md` 的白皮書修補待辦中。未回填 ⇒ 出現兩個真相來源 |

---

## M. 裁定紀錄

v0.1 的八個待裁事項已全數裁定，記錄於此以保留理由（面試素材：每一條都是一個「為什麼這樣而不是那樣」）。

| # | 事項 | 裁定 | 落在 |
|---|---|---|---|
| 1 | step 重名 | planner 的責任，決策驗收階段擋下。依據是 history 已含每步 name | B-14, B-15, D-02b |
| 2 | 重複 replay | 需冪等保護。判斷依據是 **run 的現況**，非 DLQ entry 存在與否；非 DLQ ⇒ 409 | F-08, F-10, F-13 |
| 3 | storage 未恢復 | Fail fast，且錯誤訊息必須明確指認是 storage 問題 | G-04, G-05 |
| 4 | worker 定址 | 語法不合法 ⇒ planner 錯誤；語法合法但連不上 ⇒ worker 失敗。**不引入 worker 註冊表**（見 `docs/BACKLOG.md` §2 #1） | D-08, D-09, K-13 |
| 5 | 多 run 併發 | 完全支援。run 是併發單位；不支援的是 run 內 fan-out | A-09, K-12 |
| 6 | callback 遇 DLQ run | 靠 CAS 自然擋掉，應用層不得重複檢查 | E-06 |
| 7 | crash 落在 commit 當下 | 依賴交易語意，不另寫測試。並釐清：attempt 層孤兒 vs run 層孤兒是兩件事 | C-10, C-12, C-13 |
| 12 | body 位元組穩定性（裁定 #H） | **不保證，不修。** J-01 要保護的是「無包裝、無增減欄位」，位元組穩定性從未承諾。冪等是 worker 責任，鍵是 `X-StateFlow-Step-ID`。改用 `json` 型別的代價（允許重複鍵、失去查詢與索引能力）大於收益 | J-01, K-14 |
| 8 | GET /dlq | 補齊行為；worker 側／planner 側由組合表推導，不新增欄位 | F-10 – F-13 |
| 9 | workflow name 是否唯一 | **不唯一。** name 僅為顯示標籤，不加 UNIQUE；唯一識別靠 workflow_id | N-17 |
| 10 | per-step 覆寫 retry limit | **做。** 存於 StepSpec（decision JSONB），不新增 DB 欄位；解析為 step > workflow | N-24 – N-28 |
| 11 | per-run 覆寫 retry limit 或 timeout | **不做。** 需求可由 per-step 覆寫更精確地表達；多一層只會讓「這個 X 從哪來」更難回答 | — |

新增規格領域：**N 節（config 格式與提交時驗證）**，治理原則為「寧可過度嚴謹」。

---

---

*BEHAVIOR_MATRIX v2.1 — 純行為規格。實作指示移入 session prompts，測試基礎設施移入測試 prompt，待辦移入 `docs/BACKLOG.md`，環境資訊在 `docs/OPERATIONAL_FACTS.md`。*
