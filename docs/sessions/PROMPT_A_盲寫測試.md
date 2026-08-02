# Claude Code Prompt A —— 盲寫驗收測試

> **執行環境（owner 必讀，執行前先做）**
>
> 這個 session **必須在一個看不到 StateFlow 原始碼的目錄**執行，且該目錄的**路徑結構與 repo 相同**，這樣產出的檔案可以直接複製回去。setup 腳本見本檔案末尾的附錄。
>
> **不要複製任何 `.go` 檔、不要複製 `migrations/`、不要複製 `internal/`、不要複製 `demo/`。**
> 隔離要靠實體而非指令：指令會被繞過，空目錄不會。
>
> 這個 session 若與 Prompt B 並行，兩者**必須各自使用獨立的 git worktree**（Sessions 11/12 曾因共用 checkout 導致一個 agent 的 `git checkout -b` 抽掉另一個的 HEAD）。

---

## 你的角色

你在為 **StateFlow** 撰寫黑箱驗收測試。StateFlow 是一個 Go 寫的耐久執行編排器（durable execution orchestrator）。

**你看不到它的原始碼，這是刻意的。** 你的真相來源是三份文件，**優先序如下，衝突時上位者勝**：

| 優先 | 文件 | 地位 |
|---|---|---|
| 1 | `spec/BEHAVIOR_MATRIX.md` | **權威行為規格。** 每一列都是一條應該成立的斷言 |
| 2 | `docs/WHITEPAPER_V1_1_PATCHES.md` | **白皮書的修正清單。** 標記白皮書哪些段落已經失實 |
| 3 | `docs/StateFlow_Whitepaper_v1_0.md` | 架構背景與設計理由。**已知多處落後於實作** |

> ⚠️ **白皮書 v1.0 有數處是錯的，而且錯法會讓你寫出錯誤的測試。** 例：§12.2 說 history 帶「完整 output」（實際已上界化為 2 KB／筆、50 KB 累計）；§13.2 說 `retry_after_seconds` 被忽略（實際已生效）；§18 列了八項缺口（其中六項已關閉）。
>
> **凡是白皮書的敘述與矩陣或 patches 不符 —— 以矩陣與 patches 為準，並在 `FINDINGS.md` 記一筆。**

**你不是在描述系統做了什麼，你是在釘住系統應該做什麼。** 測試失敗時，預設結論是「實作錯了」，不是「測試該放寬」。

---

## 系統速覽（讓你有足夠背景寫測試）

StateFlow 驅動多步驟 AI pipeline，每一步的決策與結果都持久化，crash 後從斷點續跑而不重跑已完成的步驟。

**四個元件：** Orchestrator（唯一的狀態寫入者）、Storage（Postgres，唯一真相）、Planner（回答「下一步做什麼」的 HTTP 端點）、Worker（實際做事的 HTTP 端點）。

**三態模型：** run 與 step 各為 `RUNNING`／`DONE`／`DLQ`；attempt 為 `RUNNING`／`DONE`／`FAILED`（FAILED 必附四值 reason 之一）。

**兩道寫入屏障：** TX1 先持久化決策才准派送 worker；TX2 先持久化結果才准問下一次 planner。

**你要打的兩個介面（都是凍結的）：** HTTP API，以及 Postgres schema（直接下 SQL 斷言）。

---

## 你的產出

### 1. 測試檔案 —— `test/acceptance/` 底下的新檔案

按行為矩陣的節分檔：

| 檔名 | 涵蓋矩陣的 |
|---|---|
| `config_validation_test.py` | N 節全部（提交時驗證、格式一致性、strict mode） |
| `worker_failure_test.py` | B 節（四種失敗 reason、預算、DLQ 進入） |
| `planner_failure_test.py` | D 節（planner 預算、三種 DLQ reason、驗收兩層） |
| `dlq_replay_full_test.py` | F 節（replay 兩條路徑、冪等 409、`GET /dlq` 過濾與分類） |
| `wire_format_test.py` | J 節（sync 裸 body + headers、async 信封、大小寫規則） |
| `invariants_test.py` | I 節（掃全庫的不變量健檢，**可獨立呼叫**） |
| `observability_test.py` | K.3（`/healthz` 兩種回應、`/ui` 純讀且不含任何 POST） |

**現有的 `crash_recovery_test.py` 與 `dlq_replay_test.py` 不要改、不要刪。** 新測試是它們的超集，退役的時機由 owner 決定，不由你決定。

### 2. 本機一鍵執行腳本 —— `scripts/test-all.sh`

必須做到：

- 起 stack、等 healthy（`stateflow` 服務已有 healthcheck，可直接等）
- 依序跑：Go 測試 → 所有 acceptance 測試 → 不變量健檢
- **每一支測試結束後自動跑一次 `invariants_test.py`** —— 不變量被破壞時，最有價值的資訊是「哪一個情境破壞了它」
- 任一失敗即以非零離場碼結束，並印出清楚的失敗摘要
- 支援 `--quick`（只跑 Go 測試與不變量健檢，跳過需要 kill 容器的慢測試）

### 3. CI 接續 —— 修改 `.github/workflows/ci.yml`

**CI 已存在，你是接進去，不是重建。** 目前兩個 job：`test`（Go 測試）與 `e2e`（compose stack + demo + 凍結 oracle）。

- 新的 acceptance 測試接進 `e2e` job
- 不變量健檢在 `e2e` 的每個階段後跑一次
- 兩個 job 維持每次 push 都觸發

### 4. `FINDINGS.md`

在這裡回報你在寫測試過程中發現的**規格問題**：矛盾、不明確、無法測試、彼此衝突的列。

**這一份的價值可能高於測試本身。** 規格的洞只有在有人試圖把它變成可執行斷言時才會現形。

---

## 硬規則

| # | 規則 |
|---|---|
| A-1 | **只用 HTTP + SQL。** 不得 import 任何 Go 型別，不得引用任何 `.go` 檔案路徑，不得假設任何內部函式名。斷言一律打在 API 回應或 DB 資料列上 |
| A-2 | **每個測試函式的 docstring 首行必須寫矩陣 ID**（如 `B-10`、`N-06`）。無 ID 的測試不該存在——它斷言的東西不在規格裡 |
| A-3 | **只用 Python 標準函式庫。** 沿用 `_harness.py` 的既有做法（`urllib.request` + `subprocess` 呼叫 `psql`）。不新增任何依賴 |
| A-4 | **不得為了讓測試通過而放寬斷言。** 你看不到實作，所以你無從得知它會不會過——這正是重點 |
| A-5 | 每支測試自己建立 workflow 與 run，**不共用狀態**。CI 上會併發執行 |
| A-6 | 矩陣 K.1 節列的是系統**明確不保證**的事。**不得為 K.1 的任何一列寫失敗測試** |
| A-7 | `_harness.py` 需要修改時（例如 `retry_limit` 移到 body 頂層），**明確在 `FINDINGS.md` 中提出並說明理由，不要直接改**。凍結檔案的編輯權屬於 owner |

---

## 特別注意的四件事

**第一，兩層未知欄位檢查是分開的。** 矩陣 N-07 是 `planner_config` 內的未知欄位，N-18 是**頂層**的未知欄位。只測一層是這類驗證最常見的漏測形態。兩者各寫一個測試。

**第二，大小寫規則有兩套，方向相反。** 送給 planner 的 history 中每個 `status` 是**大寫**（`"DONE"`）；planner 回的 StepDecision 中的 `status` 是**小寫**（`continue`／`done`／`fail`）。這是兩個不同欄位、兩套不同規則，各釘一次（J-04、J-05）。

**第三，sync 的 body 必須位元對位元等於 planner 決定的 input。** 沒有任何包裝。識別碼走 header（`X-StateFlow-Step-ID`、`X-StateFlow-Attempt-ID`）。這條壞掉時不會報錯，只會靜默破壞「零 worker 修改」的承諾（J-01、J-02）。

**第四，重試延遲窗口。** 矩陣 E-08 要求：TX3 之後、TX4 之前的任何回報都必須是 200 ACK 零效果。這個窗口從外部觸發不易——若你判斷它無法用 HTTP + SQL 測到，**在 `FINDINGS.md` 標為 L-SEED 層，不要勉強寫一個測不準的黑箱測試**。

---

## 環境：Docker Desktop / WSL2 的已知問題與正確做法

現有的 oracle 讓 fake planner／worker 跑在**主機**上，orchestrator 在**容器**裡，容器透過 `host.docker.internal` 回連主機。**這在 Docker Desktop／WSL2 拓樸下會確定性失敗**（Session 21 以 6/6 對照實驗證實，與程式碼無關）。

**已知可行的做法（Session 8.5 實測成功）：** 把 `ADVERTISE_HOST` 設為 WSL2 介面的實際 IP（例如 `172.31.72.20`），而非預設的 `host.docker.internal`。

**你要做的（優先序由高到低）：**

1. **首選 —— 讓 fakes 進容器。** 在 `docker-compose.demo.yml` 之外新增一個 `docker-compose.acceptance.yml`，把 fake planner／worker 跑成 compose 服務，orchestrator 用**容器名稱**存取它們。這樣主機↔容器的邊界整個消失，Linux／Mac／Windows／CI 行為一致。**這是真正的修法，不是規避。**
2. **次選 —— 在 `scripts/test-all.sh` 中自動偵測。** 依序嘗試 `host.docker.internal` 與 WSL2 介面 IP，取先連通者，並印出實際使用的值。
3. **無論做哪一種，都要在 `FINDINGS.md` 記錄，並產出一段可貼進 README 疑難排解的文字。** 第一個 clone 這個專案的人若看到測試是紅的，會直接認定專案是壞的——這是純粹的 usability 傷害，修補成本是三行文字。

**另一個已知的環境陷阱（Session 8.5 記錄，避免重蹈）：** 從 Git Bash 呼叫 `wsl.exe -d Ubuntu -- bash -lc '...'` 時，內嵌的 `$(...)` 命令替換會**靜默求值為空字串**而非報錯。曾導致 `ADVERTISE_HOST` 為空、planner URL 變成 `http://:7102/`、run 立刻以 `planner_unreachable` 進 DLQ。解法：把指令寫進 `.sh` 檔再執行，並加 `MSYS_NO_PATHCONV=1`。

---

## 完成條件

1. 上述七支測試檔案存在，每個測試函式的 docstring 首行都有矩陣 ID
2. `scripts/test-all.sh` 可執行，`--quick` 模式可用
3. `ci.yml` 已接上新測試
4. `FINDINGS.md` 已產出
5. **產出一張覆蓋表**：矩陣每一個 ID → 哪個檔案的哪個函式涵蓋它，或標記「L-SEED（無法黑箱測試）」／「L-SQL（不變量）」／「K.1（不保證，不測）」。**沒有任何 ID 可以沒有歸屬。**

**注意：測試現在會失敗，這是預期的。** 實作由另一個平行的 session 進行。你的工作是把規格變成可執行的斷言，不是讓它們變綠。

---

## 報告格式

1. 建立的檔案清單，每個一行說明
2. 覆蓋表（矩陣 ID → 測試位置）
3. `FINDINGS.md` 的摘要：你發現的規格問題
4. 你判斷無法黑箱測試的矩陣列，以及各自的理由
5. `_harness.py` 你認為需要 owner 修改的地方（**列出，不要改**）
6. WSL2 問題你採取的做法，以及為什麼
7. **你對哪些測試最沒把握它們正確反映了規格？** 具體說明。

---

## 附錄：Owner 的 setup 腳本

在**執行這個 session 之前**跑一次。把 `REPO` 改成你的 repo 路徑。

```bash
REPO=~/path/to/stateflow          # ← 改這裡
BLIND=~/sf-blind

rm -rf "$BLIND"
mkdir -p "$BLIND"/{docs,spec,test/acceptance,scripts,.github/workflows}

# 規格（三份，有優先序）
cp "$REPO/spec/BEHAVIOR_MATRIX.md"              "$BLIND/spec/"
cp "$REPO/docs/WHITEPAPER_V1_1_PATCHES.md"      "$BLIND/docs/"
cp "$REPO/docs/StateFlow_Whitepaper_v1_0.md"    "$BLIND/docs/"

# 既有的測試基礎設施（唯讀參考；_harness 用 HERE 相對路徑啟動兩支 fake，必須同目錄）
cp "$REPO/test/acceptance/_harness.py"          "$BLIND/test/acceptance/"
cp "$REPO/test/acceptance/fake_worker.py"       "$BLIND/test/acceptance/"
cp "$REPO/test/acceptance/fake_planner.py"      "$BLIND/test/acceptance/"
cp "$REPO/test/acceptance/README.md"            "$BLIND/test/acceptance/"
cp "$REPO/test/acceptance/crash_recovery_test.py" "$BLIND/test/acceptance/"
cp "$REPO/test/acceptance/dlq_replay_test.py"     "$BLIND/test/acceptance/"

# 既有 CI（要就地修改，所以放在正確路徑）
cp "$REPO/.github/workflows/ci.yml"             "$BLIND/.github/workflows/"

# 確認沒有任何實作洩漏進來 —— 預期輸出為空
find "$BLIND" \( -name '*.go' -o -name '*.sql' \) -print

echo "OK: $BLIND"
```

**最後那行 `find` 是安全閘門。** 有任何輸出就代表隔離破了，停下來查。

兩支既有的 oracle 也複製進來了，理由是讓盲寫 session 看得到既有的斷言風格與 harness 用法。**它們是唯讀參考，不得修改。**

### 產出回收

session 結束後，把新檔案複製回 repo：

```bash
cp "$BLIND"/test/acceptance/*.py  "$REPO/test/acceptance/"    # 注意勿覆寫兩支既有 oracle
cp "$BLIND"/scripts/test-all.sh   "$REPO/scripts/"
cp "$BLIND"/.github/workflows/ci.yml "$REPO/.github/workflows/"
cp "$BLIND"/FINDINGS.md           "$REPO/spec/MATRIX_FINDINGS_tests.md"

cd "$REPO" && git status --porcelain test/acceptance/
```

最後那行檢查：**`crash_recovery_test.py` 與 `dlq_replay_test.py` 不該出現在變更清單裡。** 出現就代表盲寫 session 動了凍結的 oracle。
