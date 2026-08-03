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
| — | `docs/OPERATIONAL_FACTS.md` | **只有操作事實**（怎麼啟動、埠號、DB 連線、容器名稱、網路拓樸）。**刻意不含任何語意契約** —— 系統對某個輸入回什麼，一律從矩陣推導 |

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

### 1. 測試基礎設施 —— 你要自己造

系統的設計是「planner 與 worker 都是外部 HTTP 端點」。**沒有它們就無法啟動任何 run**，所以 fake 不是測試的附屬品，是被測系統的必要對手方。

| 檔案 | 必須能做到 |
|---|---|
| `test/acceptance/fake_planner.py` | 依固定劇本回 continue／done／fail；**記錄每個 (run_id, history 長度) 被詢問過幾次**；回傳語法不合格的回應；回傳語意不合格的回應；刻意逾時或拒絕連線 |
| `test/acceptance/fake_worker.py` | sync 成功；async 成功（202 + 回呼）；非 2xx；2xx 但 body 非 JSON；2xx 但缺少宣告的欄位；靜默直到逾時；**以 step_id 為鍵冪等** |
| `test/acceptance/_harness.py` | HTTP 與 psql 輔助、fake 的生命週期管理 |
| `docker-compose.acceptance.yml` | 把兩支 fake 跑成 compose 服務 |

**只用 Python 標準函式庫。** 不引入 requests、pytest、flask —— 每多一個依賴，就多一道使用者跑測試的門檻。

> **為什麼 fake planner 一定要記錄呼叫次數：** 矩陣 C-14 是這個專案最核心的主張（planner 每個持久化的 step 恰被問一次），而呼叫計數是它**唯一的直接觀測手段**。沒有這個計數，那條規格就無法驗證。

### 2. 測試檔案 —— 分三層

**分層依據是「能不能只用 HTTP + SQL 觸發」，不是「寫起來方不方便」。**

| 層 | 位置 | 適用 | 涵蓋 |
|---|---|---|---|
| **黑箱** | `test/acceptance/*.py` | 只碰 HTTP API 與 SQL schema | A、B（多數）、D、F、G-01/02、J、N、O 節 |
| **種狀態** | `internal/store/`、`internal/orchestrator/` 的 Go 測試 | 情境**無法從外部觸發**，必須直接在 DB 種出中間狀態 | C-04、C-05、C-07、C-08、E-08/E-09 |
| **不變量健檢** | 一支掃全庫的腳本 | 「任何時刻都必須成立」的條件 | I 節全部 |

**你只寫黑箱層與不變量健檢。** 種狀態層需要 Go 型別，盲寫做不到——你只要在覆蓋表上標出哪些矩陣列屬於那一層，並**為每一列寫下它的斷言應該是什麼**（自然語言即可）。實作 session 會照你寫的斷言去實作那些 Go 測試，**它不得自行決定斷言內容**。

規則：黑箱測試**不得 import 任何 Go 型別、不得引用任何 `.go` 檔案路徑**。這是讓盲寫可行的硬條件。

種出來的狀態**必須是合法五組合之一**（矩陣落點 L1–L5）。種一個結構上不可能的狀態然後斷言系統會處理它，是在測試一個不存在的世界。

不變量健檢應在**每一支**黑箱測試結束後跑一次——不變量被破壞時，最有價值的資訊是「哪一個情境破壞了它」。

現有的 `internal/*` unit test **保留，但不主動投資**。它們對「面試講得出來」與「別人用得起來」兩個目標貢獻最小，不要為了提升覆蓋率去擴充它們。

按矩陣的節分檔：

| 檔名 | 涵蓋 |
|---|---|
| `config_validation_test.py` | N 節（提交時驗證、格式一致性、strict mode 兩層） |
| `worker_failure_test.py` | B 節（四種失敗 reason、預算、進 DLQ） |
| `planner_failure_test.py` | D 節（planner 預算、三種 DLQ reason、兩層驗收） |
| `dlq_replay_test.py` | F 節（replay 兩條路徑、冪等 409、`GET /dlq` 過濾與分類） |
| `crash_recovery_test.py` | C 節可黑箱觸發的部分（C-01、C-02、C-03、C-06、C-11、C-14、C-15、C-16） |
| `wire_format_test.py` | J 節（sync 裸 body + headers、async 信封、大小寫、history 上界化） |
| `invariants_test.py` | I 節（掃全庫的健檢，**必須可被其他測試獨立呼叫**） |
| `observability_test.py` | O 節（環境變數 fail-fast、`/healthz` 兩種回應、`/ui` 純讀） |
| `normal_path_test.py` | A 節、G 節可黑箱觸發的部分、H 節 |

### 3. 本機一鍵執行腳本 —— `scripts/test-all.sh`

目前 repo 的 `scripts/` 是空的，沒有 Makefile。**CI 綠而本機沒人跑得動，等於別人 clone 下來無從驗證這東西是好的** —— 這是 usability 問題，不是便利問題。

必須做到：

- 起 stack、等所有服務 healthy（用 `docker inspect -f '{{.State.Health.Status}}'`，**不要用 host 端 port 探測** —— Docker Desktop 的 port proxy 會在容器內程序真正 bind 之前就接受連線）
- 依序跑：Go 測試 → 黑箱測試 → 不變量健檢
- 每支黑箱測試後自動跑一次不變量健檢
- 任一失敗即非零離場，印出清楚的失敗摘要
- `--quick`：只跑 Go 測試與不變量健檢，跳過需要 kill 容器的慢測試

> **重要環境事實：這台機器上沒有安裝 Go**（WSL 與 Windows 都沒有）。既有的 Go 測試一直是在容器裡跑的。腳本**必須走容器**，不能假設本機有 `go`：
>
> ```bash
> docker run --rm --network stateflow_default \
>   -v "$PWD:/src" -w /src -v go-mod-cache:/go/pkg/mod \
>   -e TEST_DATABASE_URL="postgres://stateflow:stateflow@postgres:5432/stateflow?sslmode=disable" \
>   golang:1.25 go test -p 1 ./...
> ```
>
> 若本機有 `go` 則可直接用，腳本應自動偵測。`-p 1` 是必要的：多個 package 共用同一個資料庫且各自重置 schema。

### 4. CI 接續 —— 修改 `.github/workflows/ci.yml`

**CI 已存在，你是接進去，不是重建。** 目前兩個 job：`test`（Go 測試）與 `e2e`（完整 stack + demo）。

- 黑箱測試接進 `e2e` job；種狀態層與不變量健檢接進 `test` job
- 兩個 job 維持每次 push 都觸發。分成兩個不是為了跑得比較少，是**失敗時一眼看出是邏輯錯還是端到端錯**
- `e2e` job 目前呼叫已封存 oracle 的步驟已被移除，你把新測試接上去

### 5. `FINDINGS.md`

在這裡回報你在寫測試過程中發現的**規格問題**：矛盾、不明確、無法測試、彼此衝突的列。

**這一份的價值可能高於測試本身。** 規格的洞只有在有人試圖把它變成可執行斷言時才會現形。

---

## 硬規則

| # | 規則 |
|---|---|
| A-1 | **只用 HTTP + SQL。** 不得 import 任何 Go 型別，不得引用任何 `.go` 檔案路徑，不得假設任何內部函式名。斷言一律打在 API 回應或 DB 資料列上 |
| A-2 | **每個測試函式的 docstring 首行必須寫矩陣 ID**（如 `B-10`、`N-06`）。無 ID 的測試不該存在——它斷言的東西不在規格裡 |
| A-3 | **只用 Python 標準函式庫**（`urllib.request` + `subprocess` 呼叫 `psql` 即足夠）。不新增任何依賴 —— 每多一個依賴，就多一道使用者跑測試的門檻 |
| A-4 | **不得為了讓測試通過而放寬斷言。** 你看不到實作，所以你無從得知它會不會過——這正是重點 |
| A-5 | 每支測試自己建立 workflow 與 run，**不共用狀態**。CI 上會併發執行 |
| A-6 | 矩陣 K 節列的是系統**明確不保證**的事。**不得為 K 節的任何一列寫失敗測試** |
| A-7 | 請求／回應的形狀一律**從矩陣推導**。`OPERATIONAL_FACTS.md` 刻意不含這些資訊 —— 若你發現自己需要「先看看系統實際回什麼」才寫得下去，那代表**矩陣在該處不完整**：記進 `FINDINGS.md`，並照你對規格的最佳理解寫，不要去猜實作 |

---

## 特別注意的四件事

**第一，兩層未知欄位檢查是分開的。** 矩陣 N-07 是 `planner_config` 內的未知欄位，N-18 是**頂層**的未知欄位。只測一層是這類驗證最常見的漏測形態。兩者各寫一個測試。

**第二，大小寫規則有兩套，方向相反。** 送給 planner 的 history 中每個 `status` 是**大寫**（`"DONE"`）；planner 回的 StepDecision 中的 `status` 是**小寫**（`continue`／`done`／`fail`）。這是兩個不同欄位、兩套不同規則，各釘一次（J-04、J-05）。

**第三，sync 的 body 必須位元對位元等於 planner 決定的 input。** 沒有任何包裝。識別碼走 header（`X-StateFlow-Step-ID`、`X-StateFlow-Attempt-ID`）。這條壞掉時不會報錯，只會靜默破壞「零 worker 修改」的承諾（J-01、J-02）。

**第四，重試延遲窗口。** 矩陣 E-08 要求：TX3 之後、TX4 之前的任何回報都必須是 200 ACK 零效果。這個窗口從外部觸發不易——若你判斷它無法用 HTTP + SQL 測到，**在 `FINDINGS.md` 標為 L-SEED 層，不要勉強寫一個測不準的黑箱測試**。

---

## 網路拓樸

`docs/OPERATIONAL_FACTS.md` §5 有完整的實測資料，讀那一節。摘要：

**把 fake 跑成 compose 服務，用容器名稱互相存取。** orchestrator 收到的 worker／planner 位址形如 `http://fake-worker:6000/path` —— 容器**內部**的埠，不是 host published 的埠，位址裡不出現 `localhost`。

**為什麼選這條路：** 不是因為別條會壞，而是因為**它少一個變數**。容器對容器的名稱解析在 Linux／Mac／Windows／CI 上行為完全一致；跨越主機邊界的路徑會隨平台改變。

> **一則歷史更正：** 先前的專案文件宣稱 `host.docker.internal` 在 Docker Desktop／WSL2 下「確定性失效」。`OPERATIONAL_FACTS.md` §5.4 的實測**推翻了這個宣稱** —— 兩次乾淨實驗都成功。實際失效原因未確認（可能是當時的環境變數為空）。上面的建議不變，但理由是「少一個變數」，不是「另一條會壞」。

**還有一件事：** 會回呼 orchestrator 的 fake 必須撐得住 orchestrator 不在的情況 —— crash 測試期間，連服務名稱解析本身都會失敗。

## 完成條件

1. 上述九支測試檔案存在，每個測試函式的 docstring 首行都有矩陣 ID
2. `scripts/test-all.sh` 可執行，`--quick` 模式可用
3. `ci.yml` 已接上新測試
4. `FINDINGS.md` 已產出
5. **產出一張覆蓋表**：矩陣每一個 ID → 哪個檔案的哪個函式涵蓋它，或標記「種狀態層（無法黑箱觸發，附上該列的斷言敘述）」／「不變量健檢」／「K 節（不保證，不測）」。**沒有任何 ID 可以沒有歸屬。**

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
cp "$REPO/spec/BEHAVIOR_MATRIX.md"           "$BLIND/spec/"
cp "$REPO/docs/WHITEPAPER_V1_1_PATCHES.md"   "$BLIND/docs/"
cp "$REPO/docs/StateFlow_Whitepaper_v1_0.md" "$BLIND/docs/"

# 操作事實（Prompt R 的產出 —— 只有怎麼跑，沒有系統會回什麼）
cp "$REPO/docs/OPERATIONAL_FACTS.md"         "$BLIND/docs/"

# 既有 CI（要就地修改，所以放在正確路徑）
cp "$REPO/.github/workflows/ci.yml"          "$BLIND/.github/workflows/"

# 安全閘門 —— 預期三行都無輸出
find "$BLIND" \( -name '*.go' -o -name '*.sql' \) -print
find "$BLIND/test" -name '*.py' -print
ls "$BLIND"/docker-compose*.yml 2>/dev/null

echo "OK: $BLIND"
```

**三個 find／ls 都必須沒有輸出。** 有任何輸出代表實作或既有測試洩漏進來了，停下來查。

`test/acceptance/` 是**空的**——舊的 harness 與 fake 已封存，這個 session 從零重建它們。compose 檔案也不複製：它要自己寫 `docker-compose.acceptance.yml`，只依據 `OPERATIONAL_FACTS.md` 的網路拓樸資訊。

### 產出回收

```bash
cp "$BLIND"/test/acceptance/*.py     "$REPO/test/acceptance/"
cp "$BLIND"/docker-compose.acceptance.yml "$REPO/"
cp "$BLIND"/scripts/test-all.sh      "$REPO/scripts/"
cp "$BLIND"/.github/workflows/ci.yml "$REPO/.github/workflows/"
cp "$BLIND"/FINDINGS.md              "$REPO/spec/MATRIX_FINDINGS_tests.md"
```
