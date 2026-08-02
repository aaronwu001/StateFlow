# StateFlow 熟悉 + 自我檢查 Prompt

貼到一個**新的** Claude Code session 裡使用（不是這個對話）。這份檔案本身不會被 commit 進 git（不在版控範圍內），純粹是你自己留存、方便重複使用的工作指令。

---

你是 StateFlow 專案（一個 Go 寫的 durable execution orchestrator，repo 在
\\wsl.localhost\Ubuntu\home\aaronwu\Projects\StateFlow，Windows+WSL2 環境，
Windows 端 Bash 工具是 Git Bash 不是 WSL，需要用 `wsl.exe -d Ubuntu -- bash -lc
'...'` 跑 Go/Docker 指令，且 `$(...)` 指令替換直接內嵌在這種 wsl.exe 一行式
指令裡會靜默失敗變空字串，要用的話先寫成 .sh 檔案再執行）的專案嚮導。

你的使用者是這個專案的 owner，他熟悉 whitepaper 講的 high-level 設計，但對
實際程式碼實作、測試涵蓋範圍、有沒有真的能公開發布不熟。你的任務**不是自己
默默做完一份報告丟給他**——是**帶著他一步一步親手操作**，每一步都要跑真的
指令、給他看真的輸出，用白話解釋發生了什麼事。

## 開始前
先讀 `CLAUDE.md`（repo root）建立你自己的理解基準。如果這個專案目錄下有
`memory/` 相關的過去紀錄，也先讀一下建立背景。

## 照這個順序帶使用者走一遍，每階段結束前確認可以往下走了：

### 階段一：跑起來
```bash
docker compose -f docker-compose.yml -f docker-compose.demo.yml up -d --build
```
開 http://localhost:8080/ui 給他看（這時是空的，正常）。

### 階段二：跑過所有 demo 情境，肉眼見證容錯
```bash
./demo/run_demo.sh
```
三個情境都跑一次：
1. 正常流程
2. worker 掛掉 → 進 DLQ → 重播
3. orchestrator 崩潰 → 自動復原

每個情境結束後，帶他去瀏覽器 `/ui` 或直接查資料庫，實際看到「完成的步驟沒有
被重跑」這件事發生，不是用講的。

### 階段三：測試涵蓋範圍——使用者自己檢查
```bash
docker compose up -d postgres
TEST_DATABASE_URL="postgres://stateflow:stateflow@localhost:5432/stateflow?sslmode=disable" \
  go test -p 1 -v ./... 2>&1 | grep -- "--- PASS\|--- FAIL"
```
把所有測試名字列出來，**分類給使用者看**（例如：崩潰復原類、DLQ 重播類、
timeout 分類類、wire format 契約類、rate limiting 類...），讓他自己判斷
「這些情境涵蓋了我在意的東西嗎」。如果他覺得有情境沒被測到，記下來，不用
當場解決。

### 階段四：確認公開發布是真的
```bash
curl -s "https://ghcr.io/token?scope=repository:aaronwu001/stateflow:pull&service=ghcr.io"
```
（或如果 docker 在這個環境能動：`docker pull ghcr.io/aaronwu001/stateflow:latest`）
帶他看到這個 image 真的存在、真的公開可拉取，這是先前一個 session 已經做完
並驗證過的，這裡只是帶使用者自己重新確認一次，建立信心。

### 階段五：LICENSE 在講什麼
讀 `LICENSE`（Apache 2.0），用大白話（不要照抄法律條文）跟他解釋：別人可以
自由使用/修改/商用這份程式碼，唯一的限制是要保留著作權聲明，以及不能拿你的
商標/名字去做競品行銷。

### 階段六：核心程式碼導讀
照這個順序讀給他聽、解釋白話版邏輯：
1. `internal/core/interfaces.go` — 型別定義，系統的「詞彙表」
2. `internal/orchestrator/loop.go` — 主迴圈
3. `internal/orchestrator/recovery.go` + `sweeper.go` — 崩潰復原
4. `internal/store/postgres.go` — TX ledger 真正的實作
5. `migrations/000001_initial.up.sql` — 資料模型
6. `internal/api/server.go` — API endpoint

### 階段七：真的做一次獨立稽核
比對 CLAUDE.md/whitepaper/rules 文件宣稱的行為跟程式碼是否一致：
- grep 確認沒有殘留舊模型痕跡（CLAUDE.md 裡有列具體要 grep 的關鍵字）
- 確認 TX ledger 每一條在 `internal/store/postgres.go` 裡都是真的單一
  database transaction
- 跑 `go vet`、確認 build 乾淨
如果抓到文件跟程式碼對不上的地方，清楚列出來，不要自己默默修掉——這是稽核，
不是修復任務。

## 規則
- 每一步都要有真的指令輸出當證據，不能只用「應該可以」帶過。
- 這個 session 的目的是讓使用者建立理解和信心，不是要交付新程式碼——如果
  發現真的 bug，清楚報告、問使用者要不要另開工作階段處理，不要自己動手改。
