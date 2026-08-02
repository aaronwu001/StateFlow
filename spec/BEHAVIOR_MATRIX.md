# StateFlow 行為矩陣（BEHAVIOR_MATRIX）

**狀態：v0.2 — v0.1 的八項 DERIVED 已裁定並升為 SPEC；新增 N 節（config 驗證）。待 O 節裁定後升 v1.0 凍結。**

---

## 0. 這份文件是什麼

這不是新的規格。白皮書 v1.0 已經是權威規格，這份文件只是它的**索引轉換**：白皮書按*概念*組織（狀態模型一節、timeout 一節、recovery 一節），這份文件按*情境*組織（「worker 回來的路上 orchestrator 掛了會怎樣」）。

用途有三個，依重要性排序：

1. **測試計畫的來源。** 每一列的「預期結果」就是一條斷言。測試由這裡推導，不由 code 推導。
2. **讀 code 時的查核表。** 帶著這份清單去 code 裡找對應實作，找到打勾，找不到或找到多餘的東西就是 finding。
3. **面試素材的骨架。** 每一列都是一個「我預期 X，系統實際做 Y，理由是 Z」的完整敘事單位。

### 作者權與凍結規則

| 檔案 | 作者 | Claude Code 權限 |
|---|---|---|
| `spec/BEHAVIOR_MATRIX.md`（本檔） | Opus，經 owner 核可 | **唯讀，凍結** |
| `spec/MATRIX_FINDINGS.md` | Claude Code | 可寫 |
| `test/acceptance/` | owner | **唯讀，凍結** |

Claude Code 讀本檔的情境 ID，把查到的東西寫進 `MATRIX_FINDINGS.md` 中同 ID 的條目底下。兩份文件靠 ID 對齊，實體上永不接觸。

每個 session 的報告必須貼出：

```bash
git status --porcelain spec/ test/acceptance/     # 應為空
sha256sum spec/BEHAVIOR_MATRIX.md                 # 應與上個 session 一致
```

checksum 這條是防「動了又改回去」與「偷加一行註解」。

---

## 1. 欄位定義

| 欄位 | 含義 |
|---|---|
| **ID** | 穩定識別碼，永不重編號。刪除的列標 `[已作廢]` 保留原號 |
| **觸發情境** | 一句話描述發生了什麼 |
| **預期結果** | 系統必須達到的可觀測狀態。這一欄就是斷言 |
| **落點** | 事件結束後落在哪個合法組合（見下方 L1–L5） |
| **來源** | `§n` = 白皮書章節；`R§n` = Rules v3 章節；`P-Sn` = session prompts 第 n 節 |
| **信心** | `SPEC` / `DERIVED`（見下） |

### 信心等級

- **`SPEC`** — 白皮書、Rules v3 或 session prompts 有明文。可直接寫成測試。
- **`DERIVED`** — 我從不變量推導出來的，權威文件沒有直接寫。**不准直接寫成測試**，必須先由 owner 裁定，或由 Claude Code 在 code 中找到對應實作後升級為 SPEC 並補進白皮書。

> **審查提示：DERIVED 是本文件唯一的高風險區。** 我的典型失效模式不是隨機犯錯，而是「從白皮書推導出規格沒說的東西，然後寫得像規格說過一樣」。所有這類錯誤都會集中在標記 DERIVED 的列。優先審這些。

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

| ID | 觸發情境 | 預期結果 | 落點 | 來源 | 信心 |
|---|---|---|---|---|---|
| A-01 | `POST /workflows`，planner_type=static 或 http | TX-W 單一交易寫入 workflow 定義（含 planner_type + planner_config）；回傳 workflow_id | — | §19, §12.1 | SPEC |
| A-02 | `POST /workflows/{id}/runs` 帶 workflow_input | TX0 建 run（RUNNING）；回傳 run_id；啟動**恰好一個** loop goroutine | L1 | §5, §19 | SPEC |
| A-03 | loop 讀 frontier，planner 回 continue + 完整 StepSpec | TX1 單一交易：建 step（RUNNING、seq=MAX+1、attempt_count=0、decision=完整 StepSpec）＋ 建首個 attempt（RUNNING）＋ 設 current_attempt_id。**commit 之後才准派送** | L2 | §19 TX1, §2 | SPEC |
| A-04 | sync worker 回 2xx + 合法 JSON body | TX2 單一交易：attempt→DONE ＋ step→DONE ＋ 寫 output（整個 body）。**commit 之後才准問下一次 planner** | L1 | §19 TX2, §13.2 | SPEC |
| A-05 | StepSpec 指定 output_field 且該欄位存在 | output 只存該子樹，不是整個 body | L1 | §13.2 | SPEC |
| A-06 | async worker 回 202，之後 `POST /tasks/complete` 帶正確 ids | callback handler 只驗證＋推 channel＋回 200；狀態寫入由 loop 執行 TX2 | L1 | §10.4, §19 TX2 | SPEC |
| A-07 | loop 再次讀 frontier | history 為全部 DONE steps，依 `seq` 升冪，含每步完整 output | — | §12.2 | SPEC |
| A-08 | planner 回 done | TX7：run→DONE。此 run 永不再被掃描、永不加標籤 | L3 | §19 TX7, §11 | SPEC |
| A-09 | 同一 workflow 同時啟動多個 run | **完全支援。** run 是系統的併發單位；同 workflow 只代表共用同一份設定。各 run 有獨立 goroutine、獨立 seq 序列、獨立預算，互不影響 | L1/L2 | 裁定 #5 | SPEC |

---

## B. Worker 失敗（四種 reason）

| ID | 觸發情境 | 預期結果 | 落點 | 來源 | 信心 |
|---|---|---|---|---|---|
| B-01 | sync worker 回非 2xx | attempt→FAILED(`worker_reported`)，HTTPStatus 記錄於 error | L2 | §4.2, P-S4 | SPEC |
| B-02 | async worker 回呼 `/tasks/fail` | attempt→FAILED(`worker_reported`) | L2 | §13.2 | SPEC |
| B-03 | sync worker 超過 effective timeout 未回應 | attempt→FAILED(`timeout`) | L2 | §6 | SPEC |
| B-04 | async worker 回 202 後靜默超過 timeout | `select(channel, timer)` 中 timer 勝出 → attempt→FAILED(`timeout`)。**不得裸等 channel** | L2 | §6 | SPEC |
| B-05 | sync worker 回 2xx 但 body 非合法 JSON | attempt→FAILED(`malformed`) | L2 | §4.2, §13.2 | SPEC |
| B-06 | sync worker 回 2xx，但 StepSpec 宣告的 output_field 不存在 | attempt→FAILED(`malformed`) | L2 | §13.2 | SPEC |
| B-07 | async callback ids 合法但 output 不可解析 | attempt→FAILED(`malformed`) | L2 | §7.1 | SPEC |
| B-08 | async callback 缺少或帶非法 step_id/attempt_id | 回 **400**，**零狀態效果**（等該 attempt 自己的 timeout 認領） | L2 | §7.1 | SPEC |
| B-09 | 任一失敗且 attempt_count < X | TX3：attempt→FAILED(reason) ＋ count++；等 **5 秒**重試延遲；TX4：建新 attempt ＋ CAS 換 current_attempt_id；再派送 | L2 | §7.1, §6 | SPEC |
| B-10 | 失敗使 attempt_count 達到 X | **TX3 同一交易內**：attempt→FAILED ＋ count=X ＋ step→DLQ ＋ run→DLQ ＋ 插入一列 dead_letter_queue（reason=`worker_retry_exhausted`，context 含逐次 attempt 的 reason 與 error） | L4 | §19 TX3, §7.1 | SPEC |
| B-11 | B-10 之後 | **不得再派送任何 attempt**。step 與 run 皆為終態，唯一出口是 replay | L4 | §7.1, §11 | SPEC |
| B-12 | 四種 reason 分別發生 | 四者走**完全相同**的 TX3 路徑、同耗預算；分類只寫進 DB 供人閱讀，機器不依它分支 | L2/L4 | §4.2 | SPEC |
| B-13 | planner 回的 StepSpec 中 mode 不是 sync 也不是 async | 非 async 一律走 sync（multi.go fallthrough）；缺 mode 已在 planner 驗收階段被擋（→ D-02） | L2 | P-S4 | SPEC |
| B-14 | planner 回的 step name 與本 run 已存在的 step 同名 | **planner 的責任。** 在 TX1 之前的決策驗收階段擋下 → 分類為 `malformed`，耗 planner 預算（→ D-02）。**不得走到 PK 衝突**。依據：§12.2 的 history 已含每步 `name`，planner 看得到過去所有名稱，重名是它的錯 | L1 | 裁定 #1 | SPEC |
| B-15 | static planner 的 YAML 步驟表本身含重名 | **不在執行期處理。** 於 `POST /workflows` 的 config 驗證階段擋下（→ N-06）。static planner 在執行期仍維持「構造上不會失敗」 | — | 裁定 #1+#B | SPEC |

---

## C. Crash window（本文件的核心）

> 白皮書 §19 的驗收法：**相鄰兩次持久化寫入之間，每一個間隙都是一個 crash window。** 下表逐一認領。

| ID | 觸發情境 | 預期結果 | 落點 | 來源 | 信心 |
|---|---|---|---|---|---|
| C-01 | planner 已回答（continue / done / fail），但 TX1/TX7/TX8 尚未 commit 就 crash | **未持久化的答案視同沒發生。** recovery 見 run=RUNNING, last_step=DONE → 重問 planner。LLM planner 這次可能給不同答案，**合法** —— 「恰問一次」只保障已持久化的決策 | L1 | §17 Q17 | SPEC |
| C-02 | TX1 已 commit，但 worker 尚未被派送就 crash | 決定不遺失。recovery 認領孤兒 attempt（→FAILED(`orphaned`)＋count++），再由 TX4 **重派已存的 decision**，**不重問 planner** | L2 | §17 Q1, §8.3 | SPEC |
| C-03 | worker 已被派送、正在執行或結果正在回程時 crash | 同 C-02 路徑。舊 attempt 被判 `orphaned`；worker 端可能重複執行 —— 由 worker 的 step_id 冪等吸收 | L2 | §8.3, §15.1 | SPEC |
| C-04 | crash 落在 TX3 與 TX4 之間（已判失敗、新 attempt 未建） | recovery 見 step=RUNNING、last_attempt=FAILED、count<X → **沒有 RUNNING attempt 可認領**，直接走預算檢查 → TX4 建新 attempt 派送。**不得重複計入預算** | L2 | §7.1 | SPEC |
| C-05 | crash 落在 5 秒重試延遲當中 | 與 C-04 同一個窗口、同一條規則。**recovery 重派時故意跳過 5 秒延遲** —— crash 本身已提供足夠冷卻 | L2 | P-S5 | SPEC |
| C-06 | TX2 已 commit，但下一次 planner 呼叫前 crash | 已完成的 step **完全不被觸碰**（output 與 created_at 跨 crash 位元相同）；recovery 重問 planner | L1 | §8.2, §2 | SPEC |
| C-07 | crash 發生在 recovery 執行到一半 | **recovery 可重入。** 已被認領的孤兒此時是 FAILED 不是 RUNNING，不可能被二次認領、二次計數 | L2 | §8.3 | SPEC |
| C-08 | recovery 認領孤兒時 count 已是 X−1 | 孤兒認領使 count 達 X → **在 TX3 內部**直接進 DLQ；**不得派送任何新 attempt** | L4 | §8.3(b) | SPEC |
| C-09 | orchestrator 反覆 crash（crash loop） | 每次 crash 的孤兒認領耗一單位預算 → 每個 in-flight step **單調收斂到 DLQ**，無限重試在結構上不可能 | L4 | §8.3 | SPEC |
| C-10 | crash 恰好落在某個 TXn 的 commit 當下 | 交易語意：要嘛全發生、要嘛全沒發生。不存在半套狀態。**可直接依賴 Postgres，不需另寫測試** | 依 TX | §9 / 裁定 #7 | SPEC |
| C-14 | **planner 每個持久化的 step 恰被問一次**（frontier model 的核心主張） | 跨 crash、跨 recovery、跨 replay，一個已持久化的決策**永不被重新詢問**。可觀測驗證：planner 端的 decide 呼叫次數 ≤ 已建立的 step 數 + 1（最後一次問到 done／fail）。**這是與 replay family 最大的差異點，必須有直接斷言** | — | §2, §12.1 | SPEC |
| C-15 | `steps.decision` 的不可變性 | 一旦 TX1 commit，該 step 的 `decision` JSONB 與 `created_at` **永不再被寫入** —— 跨 crash 前後位元相同。recovery 重派讀的是這一欄，若它會變動，「恰問一次」的保證就是空的 | L2 | §4.2, §2 | SPEC |
| C-16 | 已完成 step 在 replay 之後 | 同 C-15：已 DONE 的 step 其 `output`、`created_at`、`decision` 在 worker 側 replay 之後仍位元相同 | L2 | §11 | SPEC |
| C-11 | 重啟後掃描 | 只掃 `runs.status='RUNNING'`（索引查找）。run=DONE 與 run=DLQ **永不被掃描、永不被觸碰** | — | §8.3 | SPEC |
| C-12 | crash 落在「TX1 commit 之後、worker 回應之前」 | 這是 C-02／C-03 的統稱：**attempt 層級的孤兒**，DB 裡留下一列 RUNNING attempt，由 recovery 判 `orphaned` | L2 | 裁定 #7 | SPEC |
| C-13 | crash 落在「已送出 planner 請求、答案未回或未持久化」 | 這是 C-01：**run 層級的孤兒，不存在孤兒 attempt**。planner 呼叫不持久化，DB 裡沒有任何東西需要認領。recovery 動作只有「重問」，**不耗 attempt 預算、不寫 failure_reason** | L1 | 裁定 #7 | SPEC |

---

## D. Planner 失敗與預算

| ID | 觸發情境 | 預期結果 | 落點 | 來源 | 信心 |
|---|---|---|---|---|---|
| D-01 | planner 超時（30s）或連線失敗 | 分類為 `unreachable`，**耗一單位 planner 預算**（總共 3 次）。這不是 attempt 失敗，不碰 attempt_count | L1 | §7.2 | SPEC |
| D-02 | planner 回應不合**語法**驗收：非合法 JSON／缺 status／continue 但缺 worker_url 或 mode／JSON 外夾散文或 markdown 圍欄 | 分類為 `malformed`，耗一單位 planner 預算 | L1 | §7.2, §12.3 | SPEC |
| D-02b | planner 回應語法合格但不合**語意**驗收：step name 與本 run 已有的重名（B-14）／mode 不是 sync 或 async／worker_url 不是合法的 http(s) URL | 同樣分類為 `malformed`，耗預算。**兩層驗收都在 TX1 之前完成**，不合格的決策永不落盤 | L1 | 裁定 #1,#3,#4 | SPEC |
| D-03 | planner 預算 3 次耗盡 | TX9：run→DLQ ＋ DLQ 紀錄，**reason = 最後一次失敗的類別**（`planner_unreachable` 或 `planner_malformed`），全部嘗試明細寫入 context | L5 | §19 TX9, §7.2 | SPEC |
| D-04 | planner 明確回 `fail` | **這是合法答案，不是失敗。** TX8：run→DLQ ＋ DLQ 紀錄（`planner_declared_fail`）。**不耗預算** | L5 | §19 TX8, §7.2 | SPEC |
| D-05 | planner 預算計數期間 orchestrator crash | 計數在記憶體，歸零。**安全，因為 planner 呼叫無副作用且在 Barrier 1 之前** | L1 | §6, §18 #3 | SPEC |
| D-06 | workflow 使用 static planner | 構造上不會失敗，**跳過整個預算路徑** | L1 | P-S5 | SPEC |
| D-07 | 每次 run 進入或重進 loop | planner 實例**每次從 workflow 那一列重建**（planner_type + planner_config），絕不從行程全域狀態取得。static 與 http 對 loop 不可分辨 | — | §12.1 | SPEC |
| D-08 | planner 回 continue，worker_url **語法不合法**（非 http/https、空字串、無法解析成 URL） | **planner 錯誤** → `malformed`，耗 planner 預算，在 TX1 之前擋下 | L1 | 裁定 #4 | SPEC |
| D-09 | planner 回 continue，worker_url 語法合法但**執行期連不上**（DNS 失敗、連線被拒、主機不存在） | **worker 失敗**，非 planner 失敗 → attempt→FAILED(`timeout`)，耗 attempt 預算。理由：這與 worker 掛掉不可分辨，必須同路 | L2 | 裁定 #4 | SPEC |

---

## E. 回報處理與 CAS

| ID | 觸發情境 | 預期結果 | 落點 | 來源 | 信心 |
|---|---|---|---|---|---|
| E-01 | 回報帶的 attempt_id ≠ steps.current_attempt_id | **200 ACK，零狀態效果**（superseded，不是 error） | 不變 | §10.1–2 | SPEC |
| E-02 | 同一 attempt 的回報重複送達兩次 | 第一次生效；第二次因 attempt 已非 RUNNING 而 CAS 失敗 → 200 零效果 | 不變 | §10.1 | SPEC |
| E-03 | 成功回報在該 attempt 已被判 FAILED(timeout) **之後**才抵達 | **一律拒絕**（MVP）。不允許 failed→done 復活轉移。工作重跑一次是可接受代價 | 不變 | §10.3, §18 #2 | SPEC |
| E-04 | 任何 callback 抵達 | callback handler 只做：驗證 → 推 channel → 回 200。**永不寫 step/run 狀態**（單一寫入者原則；第二寫入者會與 timer 競爭終局） | — | §10.4 | SPEC |
| E-05 | timeout timer 觸發後，遲到的 callback 才抵達 | timer 觸發時必須**清乾淨 channel registry**，使遲到 callback 找不到登記項而被 ACK 為 superseded | 不變 | P-S4 | SPEC |
| E-06 | callback 抵達時該 run 已經是 DLQ | **不需任何額外檢查**：該 attempt 早已非 RUNNING，CAS 自然不匹配 → 200 零效果。這是刻意把判斷交給 DB 層的設計利用，應用層不得重複實作這個檢查 | 不變 | 裁定 #6 | SPEC |
| E-08 | **重試延遲窗口內抵達的任何回報** | 舊 attempt 已是 FAILED、新 attempt 尚未建立，此期間 `steps.current_attempt_id` 指向的 attempt **狀態不是 RUNNING** ⇒ CAS 必然不匹配 ⇒ 200 ACK、**零狀態效果**。這是 E-03 的一般化：不只「超時後的成功」被拒，**冷卻期間的任何回報一律不承認、不採用、不寫入** | 不變 | 裁定 #G | SPEC |
| E-09 | 上述窗口的邊界 | 窗口自 TX3 commit 起、至 TX4 commit 止。TX4 commit 之後抵達的是**新 attempt** 的回報，帶舊 attempt_id 者仍被 CAS 擋掉（E-01） | 不變 | 裁定 #G | SPEC |
| E-10 | 此規則不得靠應用層 if 判斷實現 | 必須由 CAS 的 `AND status='RUNNING'` 條件自然達成。**若實作中出現「檢查現在是不是冷卻期」的分支，即為缺陷** —— 那是第二個判斷來源，會與 CAS 分歧 | — | 裁定 #G+#6 | SPEC |
| E-07 | CAS 的實作形式 | 必須是**單一條件式 UPDATE**：`UPDATE ... WHERE attempt_id=$1 AND status='RUNNING'`。零列受影響 ⇒ 回傳型別化的 superseded 結果，**不是 error** | — | §19 CAS-A, P-S3 | SPEC |

---

## F. DLQ 與 Replay

| ID | 觸發情境 | 預期結果 | 落點 | 來源 | 信心 |
|---|---|---|---|---|---|
| F-01 | worker 側耗盡進 DLQ | 恰好一列 dead_letter_queue，reason=`worker_retry_exhausted`，step_id 非 null，context **非空**且含逐次 attempt 的 reason 與 error | L4 | §11, §7.1 | SPEC |
| F-02 | planner 側進 DLQ | dead_letter_queue 的 step_id 為 **null**（無 step 有過錯），reason 為三種 planner 值之一 | L5 | §14.1, §11 | SPEC |
| F-03 | `POST /dlq/{id}/replay`，該 entry 是 worker 側（step_id 非 null） | TX5 單一交易：**attempt_count 歸零** ＋ step→RUNNING ＋ run→RUNNING ＋ 建新 attempt ＋ 設 current_attempt_id → 派送。**歸零是強制的**，否則 count 已等於 X，step 立刻回 DLQ，按鈕變裝飾品 | L2 | §19 TX5, §11 | SPEC |
| F-04 | `POST /dlq/{id}/replay`，該 entry 是 planner 側（step_id 為 null） | TX6：run→RUNNING → 重問 planner | L1 | §19 TX6 | SPEC |
| F-05 | replay 之後 | 已 DONE 的 steps **不得重跑**；planner 收到的 history 仍完整 | L1/L2 | §2 | SPEC |
| F-06 | `GET /runs/{id}`，run=DLQ | 回應含 `dlq_reason`（自 DLQ 表 join）；每個 step 含 status、seq、attempt_count 與最新失敗 attempt 的 reason/error 摘要 | — | P-S6 | SPEC |
| F-07 | `GET /runs/{id}` 的欄位命名 | step 時間欄位名為 `created_at`。JSON 中**任何地方都不得出現 `decided_at`** | — | P-S6, P-S1 | SPEC |
| F-08 | 對同一個 DLQ entry 連續呼叫 replay 兩次 | **第一次**的 TX5／TX6 在單一交易內把 run 帶離 DLQ（→RUNNING），這本身就是冪等閘門。**第二次**檢查 run 現況：非 DLQ ⇒ 回 **409 Conflict**，訊息明說「此 run 已在執行中」，零狀態效果。判斷依據是 **run 的現況，不是 DLQ entry 是否存在** | 不變 | 裁定 #2 | SPEC |
| F-09 | 四種 DLQ reason | 純資訊性，**replay 機制完全相同**；差別只在人的分診動作。`planner_declared_fail` 不可盲目 replay | — | §11 | SPEC |
| F-10 | `GET /dlq` 列表 | 預設只列出**目前 run.status='DLQ'** 的項目（真正待處理的人工佇列）。已被 replay 帶離 DLQ 的歷史 entry 不出現在預設列表 | — | 裁定 #2+#8 | SPEC |
| F-11 | `GET /dlq` 每一項的分類 | 由組合表推導，**不新增儲存欄位**：run=DLQ ∧ last_step=DLQ ⇒ **worker 側**；run=DLQ ∧ last_step=DONE（含無 step）⇒ **planner 側**。前者 step_id 非 null，後者為 null，兩者必須一致 | L4/L5 | 裁定 #8, §8.2 | SPEC |
| F-12 | replay 後再次失敗 | `dead_letter_queue` 會**累積第二列**（歷史紀錄永不刪除）。同一 run 可有多列 DLQ 紀錄，時間戳區分輪次。`GET /dlq` 與 replay 的查找必須容忍「一個 run 對多列」 | L4/L5 | 裁定 #2, §11 | SPEC |
| F-13 | 對「run 目前不是 DLQ」的 entry 呼叫 replay | 同 F-08：409，且訊息須指出目前的實際狀態（RUNNING／DONE），讓操作者知道發生了什麼 | 不變 | 裁定 #2 | SPEC |

---

## G. Storage 故障

| ID | 觸發情境 | 預期結果 | 落點 | 來源 | 信心 |
|---|---|---|---|---|---|
| G-01 | Postgres 掛掉 | API 拒絕新請求；每個 in-flight run 的 goroutine 在第一次寫入失敗時死亡，run 停在 RUNNING —— **孤兒但完整** | L2 | §9 | SPEC |
| G-02 | storage 恢復 ＋ orchestrator 重啟 | recovery 用**與任何 crash 完全相同的路徑**撿回這些 run | L2 | §9 | SPEC |
| G-03 | storage 故障期間的資料完整性 | **零損失、無半套狀態**。所有寫入皆為原子交易，已持久化狀態任何時刻完整一致 | — | §9 | SPEC |
| G-04 | storage 未恢復但 orchestrator 重啟 | **Fail fast。** 啟動時若無法連上 storage，orchestrator 立即以非零狀態碼退出，並在 stderr 印出**明確指出是 storage 連線問題**的訊息（不得只印通用錯誤、不得靜默重試迴圈）。整個 orchestrator 建立在「storage 完好」的假設上，這個假設不成立時唯一正確的行為是大聲地停下來 | — | 裁定 #3, §9 | SPEC |
| G-05 | 執行期 storage 斷線（非啟動時） | goroutine 於第一次寫入失敗時死亡（G-01）。錯誤日誌同樣必須可辨識為 storage 問題，而非被包裝成通用的 step 失敗 | L2 | 裁定 #3 | SPEC |

---

## H. Timeout 解析與時間規則

| ID | 觸發情境 | 預期結果 | 落點 | 來源 | 信心 |
|---|---|---|---|---|---|
| H-01 | StepSpec 有 timeout_seconds | 生效優先序：**step 覆寫 > workflow 預設 > 系統預設 60s**。timeout_seconds=0 表示向上繼承 | — | §6, P-S2 | SPEC |
| H-02 | attempt 的計時起點 | **錨定在 attempt 建立時刻，不是派送時刻。** deadline = attempt created_at + effective timeout，由 loop 算好用 `context.WithDeadline` 傳入 transport；transport 不得自起時鐘 | — | P-S4, §6 | SPEC |
| H-03 | TX1 commit 後、派送前卡住 | 由**同一支 timer** 認領 →`timeout`。「有決定、無結果」整段期間恰好被一條規則涵蓋 | L2 | §6 | SPEC |
| H-04 | 重試延遲的來源與可設定性 | **系統預設 5 秒，可由環境變數 `RETRY_DELAY_SECONDS` 覆寫**（Session 21）。格式錯誤時 fail-fast，不靜默退回預設。與 timeout 是**兩個不同旋鈕**，不可混淆 | — | 快照 S21 | SPEC |
| H-04b | worker 回報 `retry_after_seconds` 時的生效延遲 | **取大值：`max(retry_after_seconds, 系統預設)`**（Session 20）。worker 說「等更久」會被採納，說「等更短」不會 —— 系統預設是地板 | — | 快照 S20 | SPEC |
| H-04c | `retry_after_seconds` 為 0 或負數 | 取大值規則自然使其退回系統預設。**不得因此縮短延遲** | — | 快照 S20 | SPEC |
| H-04d | 重試延遲對使用者的可見性 | 不論是否開放 per-workflow 設定，**文件與 `GET /runs/{id}` 都必須讓使用者知道這段延遲存在**。否則使用者會用 `X × timeout` 估算進 DLQ 的時間，實際卻是 `X × (timeout + delay)` | — | 裁定 #G | SPEC |
| H-05 | 所有時間戳的來源 | 一律為交易 commit 當下的 DB `now()`。**絕不採用 worker/planner 回報內容中的時間，絕不用行程時鐘排序** | — | §3 | SPEC |
| H-06 | attempt 的排序依據 | `created_at`。**不存在 attempt_number 欄位** | — | §4.2 | SPEC |
| H-07 | step 的排序依據 | `seq`，且**只有** seq。不得用時間戳或 step_id 排序 | — | §4.2 | SPEC |
| H-08 | 三個數字的職責分界（**最常見的理解錯誤**） | **timeout** = 一個 attempt 的生命上限，等多久之後宣判它失敗；**retry delay** = 宣判失敗後、建立下一個 attempt 前的冷卻，固定 5s **不可設定**；**retry limit X** = 累積幾次失敗之後進 DLQ。三者互不替代 | — | §6 | SPEC |
| H-09 | 一次完整重試循環的時序 | attempt 於 T 建立 → 最遲於 T+timeout 被宣判失敗（可能更早，例如 worker 直接回 500）→ 於 T+timeout+5s 建立下一個 attempt。**最壞情況總耗時 ≈ X × (timeout + 5s)** | L2 | §6 | SPEC |

---

## N. Config 格式與提交時驗證

> **本節在白皮書中沒有對應章節，是新增的規格領域。** 治理原則（owner 裁定）：**寧可過度嚴謹，讓使用者來抱怨我們不支援某件事；也不要過度寬鬆，讓他安靜地出錯。** 所有可在提交時發現的錯誤，都不准留到執行期。

### N.1 格式一致性

| ID | 觸發情境 | 預期結果 | 來源 | 信心 |
|---|---|---|---|---|
| N-01 | 使用者提供的設定檔 | **副檔名 `.yaml` 的檔案，內容必須是真正的 YAML。** 現況「叫 .yaml 但內容是 JSON」必須修正。因為 YAML 是 JSON 的超集，既有 JSON 內容在改判為 YAML 後仍合法解析——**這是零風險遷移** | 裁定 #A | SPEC |
| N-02 | 檔案格式 vs 線上格式的分界 | **檔案端一律 YAML**（可寫註解，對 demo 與 portfolio 價值高）；**HTTP API 端一律 JSON**（`POST /workflows` 的 body）。DB 端 `planner_config` 仍為 JSONB，不需 schema 變更 | 裁定 #A | SPEC |
| N-03 | 混用 | 不得存在「副檔名與內容不符」的檔案。此為可機械檢查項 | 裁定 #A | SPEC |

### N.2 提交時驗證（`POST /workflows`）

> **治理原則（owner 裁定 #3）：輸入契約與儲存形狀是兩件事，必須分開。** API 接收的 body 有它自己的 schema，驗證只針對這份 schema；DB 的 JSONB 只是落地形狀，由 API 層正規化後寫入。**驗證器不得把 DB 的欄位形狀當成輸入契約**——那會讓儲存細節洩漏到使用者介面，並使日後改 schema 變成破壞性變更。

| ID | 觸發情境 | 預期結果 | 來源 | 信心 |
|---|---|---|---|---|
| N-04 | `planner_type` 不是 `static` 也不是 `http` | 400，訊息指出合法值 | 裁定 #B | SPEC |
| N-05 | `planner_type=http` 但 `planner_config` 缺少 URL 欄位；或 `planner_type=static` 但缺少步驟表 | 400，訊息指出缺少哪個欄位 | 裁定 #B | SPEC |
| N-06 | **交叉污染**：static planner 的 config 出現 http 專屬欄位（如 URL），或 http planner 的 config 出現 static 專屬欄位（如步驟表） | 400，訊息明說「此欄位不屬於 planner_type=X」。**這是本節最重要的一條**——這類錯誤在寬鬆實作下會被靜默忽略，使用者以為設定生效了 | 裁定 #B | SPEC |
| N-07 | `planner_config` 出現任何未知欄位 | 400（嚴格模式，不接受未知鍵）。合法鍵集合由**輸入 schema** 定義，與 DB 落地形狀無關 | 裁定 #B+#3 | SPEC |
| N-08 | static 步驟表中兩個 step 同名 | 400（承 B-15）。static planner 在執行期的「構造上不會失敗」保證，正是靠這一條在提交時成立 | 裁定 #1+#B | SPEC |
| N-09 | static 步驟表中某步驟缺 `name`／`worker_url`／`mode`，或 `mode` 不是 sync\|async，或 worker_url 語法不合法 | 400，訊息指出是第幾個步驟的哪個欄位 | 裁定 #B+#4 | SPEC |
| N-10 | `retry_limit` 缺失、非整數或 < 1；`timeout_seconds` 為負 | 400 | 裁定 #B | SPEC |
| N-11 | `retry_limit` 與 `default_timeout_seconds` 的位置 | **兩者皆為 `workflows` 表的一級欄位，且在輸入 body 中為頂層欄位。** 它們是 workflow 層級的執行參數，與「怎麼決定下一步」無關；塞進 `planner_config` JSONB 純粹是遷就舊 schema。**本輪直接改 schema，不做 API 層搬運。**詳見 N.4 | 裁定 #3+#D | SPEC |
| N-12 | 驗證失敗時的副作用 | **零副作用**：不建立 workflow、不寫任何 DB 列。驗證全部先於 TX-W | 裁定 #B | SPEC |
| N-13 | 驗證成功 | 才執行 TX-W，回傳 workflow_id | §19 | SPEC |
| N-16 | 輸入契約變更對凍結 oracle 的影響 | N-11 使 `_harness.create_workflow` 需要調整（`retry_limit` 從 `extra_config` 移到頂層 body）。**此類編輯是 owner 專屬權限，且必須引用矩陣 ID 作為理由。**Claude Code 在 session 中因測試失敗而修改 oracle，仍是紅線 | 裁定 #3 | SPEC |
| N-17 | `POST /workflows` 帶已存在的 `name` | **允許，正常建立。** `name` 僅為顯示標籤，**不加 UNIQUE 約束**；唯一識別一律靠 `workflow_id`。理由：workflow 是可重複建立的模板，調參時會自然產生同名的多個版本，強制唯一只會逼使用者發明無意義的名字 | 裁定 #E | SPEC |
| N-18 | 頂層出現未知欄位（非 `name`／`planner_type`／`retry_limit`／`default_timeout_seconds`／`planner_config`） | 400，訊息指出是哪個欄位。**與 N-07 是兩個不同層級的檢查，測試必須各釘一次**——只測其中一層是最常見的漏測形態 | 裁定 #E | SPEC |
| N-19 | 型別錯誤（`retry_limit` 給字串、`planner_config` 給陣列、`planner_type` 給數字） | 400，訊息指出欄位與期望型別。不得靜默轉型（例如把 `"2"` 當成 2） | 裁定 #E+#B | SPEC |

### N.3 Planner 對自身契約的認知

| ID | 觸發情境 | 預期結果 | 來源 | 信心 |
|---|---|---|---|---|
| N-14 | http planner 的輸出契約 | 必須有一份明確、可交付給使用者的格式規格（RunState 進、StepDecision 出，含 §12.3 語法驗收與 D-02b 語意驗收兩層）。LLM planner 附 prompt 模板 | 裁定 #C, §12.1 | SPEC |
| N-15 | planner 輸出不符該契約 | 一律歸為 planner 錯誤（`malformed`），耗預算，**絕不嘗試「猜測修正」**（不剝 markdown 圍欄、不容錯解析、不補預設值）。容錯解析會讓錯誤靜默通過，違反本節治理原則 | 裁定 #C | SPEC |

---

### N.4 Schema 變更（本輪決議）

> ⚠️ **做法已變更。** session prompts 的舊規則（就地改寫 `001_initial.sql`、不引入 migration 工具）**已於 Session 22 作廢**：專案現在使用 `golang-migrate`，檔案為 `migrations/000001_initial.{up,down}.sql`，於 `main.go` 啟動時、`RecoverRuns` 之前套用。
>
> **本輪的 schema 變更必須新增 `migrations/000002_*.{up,down}.sql`，且 up 與 down 都要寫。** 不得就地改寫 000001 —— 那會使已套用該版本的資料庫與檔案不一致。

| ID | 項目 | 內容 | 來源 | 信心 |
|---|---|---|---|---|
| N-20a | **前置查核（實作前必做）** | `default_timeout_seconds` 已在 CONFIRM 值中列為「workflow-level timeout override 的欄位名」，且 `retry_limit` 列為「在 planner_config 內的欄位名」。**實作前必須先確認這兩者目前的物理位置**（獨立欄位 vs JSONB 鍵），再決定 000002 要做什麼。若 `default_timeout_seconds` 已是欄位，本輪只需處理 `retry_limit` | 快照 §7 | SPEC |
| N-20 | `workflows` 新增欄位 | `retry_limit INT NOT NULL DEFAULT 3 CHECK (retry_limit >= 1)`、`default_timeout_seconds INT NOT NULL DEFAULT 60 CHECK (default_timeout_seconds > 0)`。理由：兩者皆非 planner 設定，且藏在 JSONB 裡**無法從 `\d workflows` 看出系統有這兩個旋鈕**——這正是可讀性問題本身 | 裁定 #D | SPEC |
| N-21 | 不另開表 | 這兩個值與 workflow 是一對一、且隨 workflow 一同建立與讀取，另開表只增加 join 成本與一致性風險。**`workflows` 表本身就是「連接到 workflow 的設定表」** | 裁定 #D | SPEC |
| N-22 | `planner_config` 的職責收窄 | 改動後 `planner_config` **只含該 planner type 專屬的設定**：`http` ⇒ `{url}`；`static` ⇒ `{steps}`。這使 N-06／N-07 的合法鍵表變成精確集合，無跨型別例外 | 裁定 #D | SPEC |
| N-23 | timeout 生效優先序（改動後） | step（StepSpec.timeout_seconds）> workflow（`workflows.default_timeout_seconds`）> 系統預設 60s。與 H-01 一致，但**現在 workflow 層真的有地方可放了**——改動前該層級在 schema 上並不存在 | §6, 裁定 #D | SPEC |
| N-24 | X 的解析優先序 | **step（StepSpec.retry_limit）> workflow（`workflows.retry_limit`）**。TX3 判斷 `count == X` 時，X 依此順序解析；step 層未指定（0 或缺欄位）⇒ 繼承 workflow。**不得從 `planner_config` JSONB 取值** | 裁定 #D+#F | SPEC |
| N-25 | per-step 覆寫 X 的儲存位置 | 存於 `steps.decision` JSONB 的 StepSpec 內，**不新增 DB 欄位**。TX3 判斷時本來就讀得到 decision，零額外查詢。**判定當下用的是落盤時的 X**——與 §4.1「DLQ 是依當時配置做出的判決」一致 | 裁定 #F | SPEC |
| N-26 | StepSpec 的 `retry_limit` 值域 | 缺欄位或 0 ⇒ 繼承 workflow；負數或非整數 ⇒ planner 語意驗收失敗（`malformed`，承 D-02b），**不落盤** | 裁定 #F | SPEC |
| N-27 | static planner 步驟表中的 per-step `retry_limit` | 同樣支援；值域錯誤在提交時驗證擋下（承 N-09） | 裁定 #F | SPEC |
| N-28 | 兩個旋鈕的對稱性 | 改動後 timeout 與 retry limit **皆為兩層覆寫**（step > workflow，timeout 再退到系統預設 60s）。此對稱性須在文件中明說，否則使用者會以為只有 timeout 可逐步調 | 裁定 #F | SPEC |

## I. 不變量（可直接寫成 SQL 斷言）

> 這一節與其他節不同：它不是情境，是**任何時刻都必須成立的條件**。適合寫成一個掃描全庫的健檢測試。

| ID | 不變量 | 來源 | 信心 |
|---|---|---|---|
| I-01 | last_attempt=DONE ⇒ step=DONE（TX2 同刀） | §8.1 | SPEC |
| I-02 | attempt_count=X ⇒ step=DLQ ∧ run=DLQ（TX3 同刀） | §8.1 | SPEC |
| I-03 | run=DONE ⇒ last_step=DONE | §8.1 | SPEC |
| I-04 | last_step=DLQ ⇒ run=DLQ | §8.1 | SPEC |
| I-05 | 不存在 status=FAILED 而 failure_reason 為 NULL 的 attempt（DB CHECK 亦應擋下） | §14.1, oracle | SPEC |
| I-06 | step.status='DONE' ⇔ output IS NOT NULL。**若兩者分歧（實作缺陷），以 output 為準** | §4.1, §19 | SPEC |
| I-07 | **不可能組合**：run=DONE 而 last_step 為 RUNNING 或 DLQ | §8.2 | SPEC |
| I-08 | **不可能組合**：run=RUNNING 而 last_step=DLQ | §8.2 | SPEC |
| I-09 | 同一 run 至多一個 status=RUNNING 的 step（序列 loop 不變量） | P-S3 | SPEC |
| I-10 | 每個完成的 step 至多一個 status=DONE 的 attempt（無重複 checkpoint） | oracle | SPEC |
| I-11 | attempt_count 只由 TX3 遞增、只由 TX5 歸零，其他任何地方不得寫 | P-checklist | SPEC |
| I-12 | 資料庫中不存在 DECIDED、step 層的 FAILED、attempt_number、decided_at、dispatched_at、replay_round | P-S8 | SPEC |
| I-13 | **現況表唯一、歷史表可累積。** `runs` 每個 run 恰好一列（run_id 為 PK）、`steps` 每個 step 恰好一列——它們記錄的是**當下狀態**。`dead_letter_queue` 同一個 run 可有多列——它記錄的是**歷史事件**。兩者職責不得混淆 | 裁定 #4, §14.1 | SPEC |
| I-14 | 「這個 run 現在是不是 DLQ」的唯一判斷依據是 `runs.status` 與 `steps.status`，**絕不是 `dead_letter_queue` 裡有沒有列**。任何讀取路徑若只查 DLQ 表就下結論，即為缺陷 | 裁定 #4 | SPEC |

---

## J. 線上格式（wire format，位元級契約）

| ID | 觸發情境 | 預期結果 | 來源 | 信心 |
|---|---|---|---|---|
| J-01 | 派送 sync worker | POST body 是**裸的 input，位元對位元**等於 planner 決定的內容，**無任何包裝** | §13.1 | SPEC |
| J-02 | 派送 sync worker | 必帶 header `X-StateFlow-Step-ID` 與 `X-StateFlow-Attempt-ID`，值正確 | §13.1 | SPEC |
| J-03 | 派送 async worker | POST body 是信封 `{step_id, attempt_id, input}` | §13.1 | SPEC |
| J-04 | 送給 planner 的 RunState | history 中**每一個 status 字串皆為大寫**（"DONE"），與儲存值一致 | §12.2 | SPEC |
| J-05 | planner 回的 StepDecision | 其 `status` 為**小寫**（continue/done/fail）。J-04 與 J-05 是兩個不同欄位、兩套不同大小寫規則，測試必須各釘一次 | §12.3, P-S6 | SPEC |
| J-06 | 送給 planner 的 history 順序 | 依 seq 升冪 | §12.2 | SPEC |
| J-07 | `/tasks/fail` 帶 `retry_after_seconds` | **可選，接受，且已生效**（Session 20 起不再忽略）：作為重試延遲的下限，見 H-04b。白皮書 §13.2／§18 #5 的「接受但忽略」敘述已過時 | 快照 S20 | SPEC |

---

## K. 明確**不**保證的事（護欄區）

> **用法：往上面任何一節新增預期行為之前，先查這一節。** 命中的話它不是 bug，是已登記的缺口，**不得寫成失敗測試**。
>
> ⚠️ **本節已依 `STATE_SNAPSHOT.md`（Session 22，`main` @ `adb24a4`）重寫。** 白皮書 §18 的八項登記缺口中，**六項已於 Phase 1.5／Phase 2 關閉**，白皮書 §18 本身尚未更新（見 P.1 #9）。凡標 ✅ 者代表「已實作，現在是可測試的行為，不再是護欄」。

### K.1 仍然成立的護欄（真正不保證的事）

| ID | 不保證的事 | 實際承諾 | 來源 |
|---|---|---|---|
| K-01 | **step 內部的 exactly-once** | 只保證 step 間 exactly-once（完成的 step 永不重跑）。同一 step_id 最壞情況**同時有 X 路併發重複呼叫**（timeout 誤殺可能與仍活著的 worker 賽跑，不只 crash 重派）。去重是 worker 責任，建議以 step_id 為鍵 | §15.1 |
| K-03 | 遲到的成功結果會被回收 | 超時後抵達的成功一律丟棄，工作重跑。**這是 §18 唯一仍未關閉的登記項（#2）**，白皮書自己標為 "2+" | §18 #2 |
| K-04 | planner 重試計數跨 crash 保留 | 記憶體計數，crash 歸零。無副作用故安全 | §18 #3 |
| K-10 | 多副本 | **仍僅支援單一 replica** —— 多個 orchestrator 的 recovery 掃描與 sweeper 會重複認領。Phase 3 以 executor-ID ownership 解決。**注意：Session 18 的 sweeper 使這條更重要了**，因為現在每 30 秒就掃一次，多副本的衝突頻率遠高於過去「只在啟動時掃一次」 | §21 |
| K-11 | 認證授權 | 完全沒有。生產環境須置於 gateway/mesh 之後 | §15.5 |
| K-12 | **單一 run 內的 fan-out** | 一 run 一 goroutine、step 嚴格序列。planner 一次只能決定一個 step，不能一次派出多個 worker。**多個 run 併發是完全支援的**（A-09），不支援的只有 run 內平行 | §5, 裁定 #5 |
| K-13 | **具名 worker 註冊表** | planner 直接給完整 `worker_url`，系統沒有已知 worker 的清單，因此不存在「查無此 worker」。語法檢查歸 planner（D-08），連不上歸 worker（D-09） | 裁定 #4 |

### K.2 已關閉的登記項（**不再是護欄；現在是可測試的行為**）

> 這些原本寫在 §18，現在都已實作。**它們的行為尚未被本矩陣完整涵蓋** —— 每一項後面標出需要補的情境，這是本輪最大的覆蓋缺口。

| 原 §18 # | 項目 | 現況 | 矩陣需補的行為 |
|---|---|---|---|
| #1 | 全史傳輸 | ✅ **已上界化**（Session 19）：單筆 `HistoryEntry.Output` 超過 2KB 轉為指標物件；**保留的** Output 累計上限 50KB，由最新往回走，超出者整個丟掉 `Output` | 需補：截斷觸發點、指標物件的形狀、最新優先的走訪方向（**方向寫反會靜默反轉整個功能**）、累計上限只計 Output 位元組不含結構開銷 |
| #4 | storage 孤兒等重啟 | ✅ **已關閉**（Session 18）：行程內 sweeper，**每 30 秒**掃一次（可由 `SWEEP_INTERVAL_SECONDS` 覆寫），storage 短暫故障後不需重啟即可回收 | 需補：sweeper 的回收路徑與 crash recovery 路徑是同一條嗎？重複掃描不會二次認領？G-01／G-02 的敘述需依此改寫 |
| #5 | `retry_after_seconds` 被忽略 | ✅ **已實作**（Session 20）：生效延遲 = `max(worker 回報的 retry_after_seconds, 系統預設)` | 需補：取大值規則、零／負值的行為。**J-07 與 H-04 原本的敘述已過時** |
| #6 | main.go 硬編組裝 | ✅ **已關閉**（Session 21）：`RETRY_MAX_ATTEMPTS`／`RETRY_DELAY_SECONDS`／`SWEEP_INTERVAL_SECONDS` 三個環境變數，預設值等同原硬編值，**格式錯誤時 fail-fast 而非靜默退回預設** | 需補：三個變數各自的預設與值域、fail-fast 行為 |
| #7 | init-only migration | ✅ **已關閉**（Session 22）：改用 `golang-migrate`，檔案為 `migrations/000001_initial.{up,down}.sql`，於 `main.go` 啟動時、`RecoverRuns` 之前套用 | **N.4 的做法必須改**：不再是就地改寫 001，而是新增 `000002_*.{up,down}.sql` |
| #8 | 無 healthcheck | ✅ **已關閉**（Session 10）：`GET /healthz`（ping Postgres，3s，200/503，純讀）＋ `stateflow healthcheck` CLI 子指令（distroless 無 shell 也能用）；Dockerfile 與 compose 皆已接上 | 需補：/healthz 的兩種回應、CLI 子指令的離場碼 |

### K.3 Phase 1.5 已交付、矩陣尚未涵蓋的表面

| 項目 | 現況 | 需補 |
|---|---|---|
| `GET /ui` | ✅ 已實作（Session 12）：單一內嵌 HTML，純讀，只呼叫 `GET /runs/{id}` 與 `GET /dlq`，無任何寫入路徑 | 需補：純讀不變量（頁面不得有任何 POST）、它讀的欄位必須與 F-06 的回應形狀一致 |
| CI | ✅ 已實作（Session 9）：`.github/workflows/ci.yml`，兩個 job（`test` 與 `e2e`） | 見 Q 節 |
| README | ✅ 已重寫（Session 11），quickstart 每一行都對活的 stack 跑過 | usability 的主要載體，變更 API 形狀時必須同步 |

---

## R. 測試環境的自足性要求

> **背景：** 舊的 acceptance 測試（`_harness.py` 與兩支 fake）與 demo 已全數封存。新測試從零重寫，**必須自帶它需要的一切**。本節定義「自帶」的邊界。

| ID | 要求 |
|---|---|
| R-01 | 測試套件必須**自行提供 fake planner 與 fake worker**。系統的設計是「planner 與 worker 都是外部 HTTP 端點」，沒有它們就無法啟動任何 run。這不是測試的附屬品，是被測系統的必要對手方 |
| R-02 | fake planner 必須能：依固定劇本回答 continue／done／fail；**記錄每個 (run_id, history 長度) 被詢問的次數**（C-14 的觀測手段）；刻意回傳不合格的回應（供 D-02／D-02b 使用）；刻意逾時或拒絕連線（供 D-01 使用） |
| R-03 | fake worker 必須能：成功回應（sync 與 async 兩種）；回非 2xx；回 2xx 但 body 非 JSON；回 2xx 但缺少宣告的 output_field；靜默不回應直到逾時；**以 step_id 為鍵做冪等**（示範 §15 的 worker 契約） |
| R-04 | **只用 Python 標準函式庫。** 不引入 requests、pytest、flask 等任何外部依賴 —— 增加使用者跑測試的門檻，違背 usability 目標 |
| R-05 | **網路拓樸必須在容器內解決。** fake planner／worker 跑成 compose 服務，orchestrator 以容器名稱存取。不得依賴 `host.docker.internal`（在 Docker Desktop／WSL2 下確定性失效，Session 21 已證實） |
| R-06 | 測試撰寫者取得的「操作事實」僅限：如何啟動 stack、服務名稱與埠、DB 連線字串、容器名稱、環境變數。**不得取得任何語意契約**（請求／回應的欄位形狀、狀態機行為）—— 那些一律來自本矩陣。混淆這條界線會使盲寫失去意義 |

---

## Q. 測試分層與執行方式

> **每一列行為都要指定它的測試落在哪一層。** 這一節定義層級與判準，避免所有東西被硬塞進黑箱測試（有些情境從外部根本觸發不了）。

### Q.1 三層

| 層 | 位置 | 適用判準 | 涵蓋的節 |
|---|---|---|---|
| **L-BLACK**（黑箱驗收） | `test/acceptance/*.py` | 只碰 HTTP API 與 SQL schema 兩個凍結介面。**不需要知道 Go 這邊長什麼樣**，因此可由完全看不到 code 的 session 撰寫 | A、B（多數）、D、F、G-01/02、J、N、K.2/K.3 的新表面 |
| **L-SEED**（種狀態的整合測試） | `internal/store/`、`internal/orchestrator/` 的 Go 測試 | 情境**無法從外部觸發**，必須直接在 DB 種出特定中間狀態。典型：crash 落在兩個 TX 之間、預算邊界、recovery 可重入 | C-04、C-05、C-07、C-08、E-08/E-09、I 節的部分 |
| **L-SQL**（不變量健檢） | 一支掃全庫的腳本，可在任何測試之後跑 | 「任何時刻都必須成立」的條件，不綁定特定情境。**成本極低、抓 bug 效率極高，建議最先做** | I 節全部 |

### Q.2 分層規則

| ID | 規則 |
|---|---|
| Q-01 | 每一列行為在 `MATRIX_FINDINGS.md` 中必須標出它屬於哪一層。**判定依據是「能不能只用 HTTP + SQL 觸發」，不是「寫起來方不方便」** |
| Q-02 | L-BLACK 的測試**不得 import 任何 Go 型別、不得引用任何 Go 檔案路徑**。這是讓它能由盲寫 session 產出的硬條件 |
| Q-03 | L-SEED 的測試種出的狀態**必須是合法五組合之一**（§8.2）。種出一個結構上不可能的狀態然後斷言系統會處理它，是在測試一個不存在的世界 |
| Q-04 | L-SQL 的健檢在**每一支** L-BLACK 測試結束後都應該跑一次 —— 不變量被破壞時，最有價值的資訊是「哪一個情境破壞了它」 |
| Q-05 | 現有的 `internal/*` unit test 保留，但**不主動投資**。它們對「面試講得出來」與「別人用得起來」兩個目標貢獻最小 |

### Q.3 現有 CI 的接續

> CI 已存在（Session 9，`.github/workflows/ci.yml`），兩個 job。新測試是**接進去**，不是從零建。

| ID | 現況與規則 |
|---|---|
| Q-06 | `test` job：對 docker-compose 起的 Postgres 跑 `go test -p 1 ./...`。**L-SEED 與 L-SQL 接這裡**，秒級 |
| Q-07 | `e2e` job：起完整 compose stack（含 demo overlay），跑 `crash_demo.py` 與兩支凍結 oracle。**L-BLACK 接這裡**，分鐘級（要 `docker kill` 容器再重啟，有 120 秒等待） |
| Q-08 | 兩個 job 都在**每次 push** 觸發（加上對 `main` 的 PR）。分成兩個 job 是為了失敗時一眼看出是邏輯錯還是端到端錯，**不是為了跑得比較少** |
| Q-09 | **必須提供本機一鍵指令**跑完整套（`make test` 或 `./scripts/test-all.sh`）。CI 綠而本機沒人跑得動，是 usability 的反面 |
| Q-10 | **已知本機限制：** `crash_recovery_test.py` 在 Docker-Desktop/WSL2 拓樸下會確定性失敗（`host.docker.internal` 轉發問題，Session 21 已用 6/6 對照實驗證實與程式碼無關）。CI 的 Linux-native 拓樸不受影響。**此事必須寫進 README 的疑難排解**，否則第一個 clone 的人會以為專案是壞的 |

---

## L. 完整性自檢清單

> **這是 owner 檢查「Opus 有沒有漏東西」的機械方法。** 每一條都可以純機械核對，不需要理解設計。

| # | 檢查 | 判準 |
|---|---|---|
| L-1 | **TX ledger 覆蓋** | §19 的 13 個項目（TX-W、TX0–TX9、CAS-A）每一個都至少出現在一列的「預期結果」中。清點：TX-W→A-01；TX0→A-02；TX1→A-03；TX2→A-04；TX3→B-09/B-10；TX4→B-09；TX5→F-03；TX6→F-04；TX7→A-08；TX8→D-04；TX9→D-03；CAS-A→E-07 |
| L-2 | **attempt failure reason 覆蓋** | 四值各至少一列：worker_reported→B-01/B-02；timeout→B-03/B-04；malformed→B-05/B-06/B-07；orphaned→C-02 |
| L-3 | **DLQ reason 覆蓋** | 四值各至少一列：worker_retry_exhausted→B-10；planner_unreachable/planner_malformed→D-03；planner_declared_fail→D-04 |
| L-4 | **合法組合覆蓋** | L1–L5 每一個都至少出現在一列的「落點」 |
| L-5 | **不可能組合覆蓋** | §8.2 的兩個不可能組合各有一列不變量（I-07、I-08） |
| L-6 | **Registry 覆蓋** | §18 的 8 個項目在 K 節各有一列（K-02 至 K-09） |
| L-7 | **crash window 覆蓋** | 列出所有相鄰持久化寫入對，每一對有一列 C-。目前對：run 建立↔TX1（C-01）、TX1↔派送（C-02）、派送↔回報（C-03）、TX3↔TX4（C-04/C-05）、TX2↔下次 planner（C-06） |
| L-8 | **API endpoint 覆蓋** | 七個 endpoint 各至少一列：POST /workflows→A-01；POST /workflows/{id}/runs→A-02；GET /runs/{id}→F-06；/tasks/complete→A-06；/tasks/fail→B-02；GET /dlq→（**目前無列，缺口**）；POST /dlq/{id}/replay→F-03/F-04 |
| L-9 | **模組覆蓋** | 六個 package 各至少被一列的行為涉及：api、orchestrator、planner、transport、store、core |
| L-8b | **GET /dlq 覆蓋** | v0.1 的 L-8 檢查抓出此缺口，v0.2 已補：F-10、F-11、F-12、F-13 |
| L-10 | **DERIVED 清點** | **v0.2：DERIVED 歸零。** v0.1 的 8 列（A-09、B-14、C-10、D-08、E-06、F-08、G-04 與 GET /dlq 缺口）已全部由 owner 裁定升為 SPEC。新增的 N 節全部標 SPEC，因為它們是裁定產物，不是推導產物 |
| L-11 | **裁定回填白皮書** | N 節與 D-02b 是白皮書目前沒有的規格。核對：白皮書 §12.3 是否已擴充為兩層驗收？是否已新增 config 驗證章節？**未回填 ⇒ 矩陣正在變成第二個真相來源，這是必須擋下的漂移。**待辦集中在 P 節 |
| L-12 | **凍結 oracle 相容性** | 分層原則：**spec → 測試 → code**，上層可以推翻下層。但 oracle 的編輯權**專屬 owner**，且必須引用矩陣 ID 作為理由。檢查：本輪是否有 oracle 需要調整？（目前已知一項：N-16） |
| L-13 | **未實作功能不得入矩陣** | 矩陣的列是「會被寫成測試的斷言」。尚未實作的東西寫成列，會讓「測試失敗＝有 bug」這個判斷失效。未實作項目一律放 P 節，實作後才升格為矩陣列 |

---

## M. 裁定紀錄

v0.1 的八個待裁事項已全數裁定，記錄於此以保留理由（面試素材：每一條都是一個「為什麼這樣而不是那樣」）。

| # | 事項 | 裁定 | 落在 |
|---|---|---|---|
| 1 | step 重名 | planner 的責任，決策驗收階段擋下。依據是 history 已含每步 name | B-14, B-15, D-02b |
| 2 | 重複 replay | 需冪等保護。判斷依據是 **run 的現況**，非 DLQ entry 存在與否；非 DLQ ⇒ 409 | F-08, F-10, F-13 |
| 3 | storage 未恢復 | Fail fast，且錯誤訊息必須明確指認是 storage 問題 | G-04, G-05 |
| 4 | worker 定址 | 語法不合法 ⇒ planner 錯誤；語法合法但連不上 ⇒ worker 失敗。**不引入 worker 註冊表**（見 P.2 #1） | D-08, D-09, K-13 |
| 5 | 多 run 併發 | 完全支援。run 是併發單位；不支援的是 run 內 fan-out | A-09, K-12 |
| 6 | callback 遇 DLQ run | 靠 CAS 自然擋掉，應用層不得重複檢查 | E-06 |
| 7 | crash 落在 commit 當下 | 依賴交易語意，不另寫測試。並釐清：attempt 層孤兒 vs run 層孤兒是兩件事 | C-10, C-12, C-13 |
| 8 | GET /dlq | 補齊行為；worker 側／planner 側由組合表推導，不新增欄位 | F-10 – F-13 |

新增規格領域：**N 節（config 格式與提交時驗證）**，治理原則為「寧可過度嚴謹」。

---

## P. 待辦集中區

> **本節是本檔案唯一的「尚未成立」區。** 上面所有節都是「現在就該成立、可以直接寫成測試」的行為；這一節是白皮書修補待辦，以及我們預計未來可能需要的東西。不標 phase——只記錄內容與理由，排程另行決定。消化進白皮書之後，對應項目從這裡刪除。

### P.1 白皮書修補待辦（已裁定，但白皮書還沒寫）

| # | 需要修補的地方 | 內容 |
|---|---|---|
| 1 | §12.3 決策驗收標準 | 擴充為**兩層**：語法驗收（現有內容）＋語意驗收（step 重名、mode 值域、worker_url 語法）。兩層都在 TX1 之前，都歸 `malformed` |
| 2 | 新增章節：Config 與提交時驗證 | 整個 N 節。含輸入契約 vs 儲存形狀的分離原則、嚴格模式、YAML/JSON 分界、頂層與 planner_config 兩層檢查 |
| 3 | §11 DLQ 與 replay | 補上 replay 冪等規則（判斷依據是 run 現況非 entry 存在）、`GET /dlq` 的預設過濾、DLQ 表可累積多列 |
| 4 | §9 Storage | 補上啟動時 fail-fast 與錯誤訊息可辨識性的要求 |
| 5 | §4.2 或 §14.1 | 明文寫出「現況表唯一、歷史表可累積」的職責分界（I-13、I-14） |
| 6 | §15 使用者契約 | 補上：config 錯誤在提交時就會被拒絕，不會留到執行期 |
| 7 | **§14.1 schema** | `workflows` 加 `retry_limit` 與 `default_timeout_seconds` 兩個一級欄位（N-20）。§6 的「workflow 層可覆寫 timeout」在改動前於 schema 上不存在，改動後才名實相符 |
| 8 | **§6 timeout 兩個旋鈕** | 補上 X 的覆寫層級說明（per-step 覆寫已裁定要做）；補上 retry delay 可由 `RETRY_DELAY_SECONDS` 覆寫、且 worker 的 `retry_after_seconds` 取大值 |
| 9 | **§18 Temporary Design Registry（最急）** | **八項中有六項已關閉**（#1 上界化、#4 sweeper、#5 rate limiting、#6 config 組裝、#7 migration、#8 healthz），白皮書仍寫著它們是缺口。這是目前文件與現實落差最大的一節 —— 任何讀白皮書的人（包括面試官）都會低估這個專案的完成度 |
| 10 | **§12.2 全史傳輸** | 已上界化（2KB/筆、50KB 累計、最新優先）。§12.2 仍寫「carries each step's full output」 |
| 11 | **§13.2 `retry_after_seconds`** | 已生效，不再是「接受但忽略」 |
| 12 | **§9 Storage / §8.3 Recovery** | 補上行程內 sweeper（30 秒間隔）：storage 短暫故障後不需重啟即可回收孤兒 |
| 13 | **新增：可觀測表面** | `GET /healthz`、`GET /ui` 目前完全不在白皮書中 |

### P.2 預計未來可能需要的東西

| # | 項目 | 為什麼可能需要 | 已知代價 |
|---|---|---|---|
| 1 | **具名 worker 註冊表** | workflow config 宣告具名 worker，planner 回傳名稱而非完整 URL。好處：LLM planner 無法憑空捏造 endpoint；設定集中；換環境不用改 planner | 需與 `worker_url` 並存（擇一）以免破壞現有契約；會動到凍結 oracle 的 `fake_planner.py` |
| 2 | ~~`/healthz` 端點~~ | ✅ **已完成**（Session 10）。k8s 的硬前置條件已解除 | — |
| 3 | ~~狀態 UI~~ | ✅ **已完成**（Session 12，`GET /ui`）。可考慮的下一步：讓它能即時看到 frontier 前進與 recovery 撿回的過程（目前是靠使用者手動重新整理） | — |
| 4 | **失敗原因命名精確度** | 連線被拒（瞬間發生）目前與真正的逾時共用 `timeout` 這個 reason，人工分診時有誤導性。錯誤字串裡有真相，但 reason 欄位沒有 | 改動四值列舉＝改 DB CHECK＋改凍結 oracle 的斷言。**代價明顯大於收益，傾向不做，只在文件說明** |
| 5 | **workflow 層級 timeout 預設值的輸入位置** | 見 N-20a：需先查核 `default_timeout_seconds` 目前是欄位還是 JSONB 鍵 | 與 N.4 同一次改動 |
| 7 | **retry delay 開放 per-workflow 設定** | 目前只有行程層級的 `RETRY_DELAY_SECONDS`，所有 workflow 共用。若不同 workflow 的 worker 恢復特性差很多，會想分開設。**但即使不開放，H-04d 要求它必須對使用者可見** | 與 N.4 同一次 schema 改動可順帶 |
| 8 | **Kubernetes / Helm** | `/healthz` 已就緒，硬前置條件解除。單一 replica 限制（K-10）必須在部署文件中明講 | 快照確認 `/healthz` 與 CLI 子指令都可用 |
| 6 | **`GET /dlq` 的含歷史模式** | 供稽核用（`?include_replayed=true`）。目前預設只列現況 DLQ | 低，但非必要 |

---

### P.3 本輪裁定紀錄（已解決，不再待裁）

| # | 事項 | 裁定 | 落在 |
|---|---|---|---|
| 1 | workflow name 是否唯一 | **不唯一。** name 僅為顯示標籤，不加 UNIQUE；唯一識別靠 workflow_id。凍結 oracle 因此不受影響 | N-17 |
| 2 | per-step 覆寫 retry limit | **做，本輪一起做。** 存於 StepSpec（decision JSONB），不新增 DB 欄位；解析為 step > workflow | N-24 – N-28 |
| 3 | per-run 覆寫 | **不做。** 需求可由 per-step 覆寫更精確地表達，多一層只會讓「這個 X 從哪來」更難回答 | — |

---

*BEHAVIOR_MATRIX v1.2 — 新增 C-14/C-15/C-16（planner 恰問一次的可觀測斷言、decision 不可變性）與 R 節（測試環境自足性）。v1.1 — 依 STATE_SNAPSHOT（Session 22, `main` @ `adb24a4`）修正 K 節與 N.4；新增 Q 節（測試分層與 CI）、重試延遲窗口規則（E-08–E-10）、retry delay 可設定性（H-04 系列）。*
