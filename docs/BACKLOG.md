# StateFlow Backlog

> **這裡放「還沒成立的事」。** 行為矩陣（`spec/BEHAVIOR_MATRIX.md`）只放「現在就該成立、可以直接寫成測試」的行為；這個檔案放白皮書修補待辦與未來可能要做的東西。
>
> **不標 phase** —— 只記錄內容與理由，排程另行決定。消化完成的項目從這裡刪除。

---

## 1. 白皮書修補待辦（已裁定，但白皮書還沒寫）

| # | 位置 | 內容 |
|---|---|---|
| 1 | §18 Temporary Design Registry（**最急**） | 八項中六項已關閉（#1 上界化、#4 sweeper、#5 rate limiting、#6 config 組裝、#7 migration、#8 healthz），白皮書仍寫著它們是缺口。**任何讀白皮書的人都會嚴重低估專案完成度** |
| 2 | §12.2 全史傳輸 | 已上界化（2 KB／筆、50 KB 累計、最新優先）。§12.2 仍寫「carries each step's full output」 |
| 3 | §13.2 `retry_after_seconds` | 已生效（取大值），不再是「接受但忽略」 |
| 4 | §6 timeout | retry delay 可由 `RETRY_DELAY_SECONDS` 覆寫，非固定值；補上 X 的兩層覆寫；補上三個數字的分界說明 |
| 5 | §9 Storage / §8.3 Recovery | 行程內 sweeper 已實作（30 秒間隔），非提案；補上啟動時 fail-fast 的要求 |
| 6 | §12.3 決策驗收 | 擴充為語法／語意兩層 |
| 7 | 新增章節：Config 與提交時驗證 | 整個 N 節。含輸入契約 vs 儲存形狀的分離原則、嚴格模式、YAML/JSON 分界 |
| 8 | §14.1 Schema | `workflows` 新增 `retry_limit`／`default_timeout_seconds` 一級欄位；per-step X 存於 decision |
| 9 | §11 DLQ 與 Replay | replay 冪等（409）、`GET /dlq` 預設過濾、現況表與歷史表職責分界 |
| 10 | §10 回報處理 | 冷卻窗口內回報一律不生效 |
| 11 | 新增：可觀測表面 | `GET /healthz`、`GET /ui` 目前完全不在白皮書中 |
| 12 | §21 Roadmap | Phase 1.5／2 交付狀態更新 |
| 13 | §15 使用者契約 | 補上：config 錯誤在提交時就會被拒絕，不會留到執行期 |
| 14 | §12.1 Planner 契約 | 需要一份可交付給使用者的完整格式規格（RunState 進、StepDecision 出，含語法／語意兩層驗收），LLM planner 附 prompt 模板 |
| 15 | Q&A | 建議增補三題：三個數字的差別、為什麼 config 錯誤要在提交時拒絕、為什麼 `retry_limit` 搬出 `planner_config` |

---

## 2. 預計未來可能需要的東西

| # | 項目 | 為什麼可能需要 | 已知代價 |
|---|---|---|---|
| 1 | **具名 worker 註冊表** | workflow config 宣告具名 worker，planner 回傳名稱而非完整 URL。好處：LLM planner 無法憑空捏造 endpoint；設定集中；換環境不用改 planner | 需與 `worker_url` 並存（擇一）以免破壞現有契約 |
| 2 | **失敗原因命名精確度** | 連線被拒（瞬間發生）目前與真正的逾時共用 `timeout` 這個 reason，人工分診時有誤導性 | 改四值列舉＝改 DB CHECK＋改所有相關斷言。**代價大於收益，傾向不做，只在文件說明** |
| 3 | **retry delay 開放 per-workflow 設定** | 目前只有行程層級的 `RETRY_DELAY_SECONDS`，所有 workflow 共用 | 與 schema 改動可順帶 |
| 4 | **`GET /dlq` 的含歷史模式** | 供稽核用（`?include_replayed=true`）。目前預設只列現況 DLQ | 低，但非必要 |
| 5 | **Kubernetes / Helm** | `/healthz` 已就緒，硬前置條件解除 | 單一 replica 限制（K-10）必須在部署文件中明講 |
| 6 | **UI 即時更新** | 目前靠使用者手動重新整理。能即時看到 frontier 前進與 recovery 回收的畫面，是讓陌生人三十秒看懂這系統的最直接手段 | 中等 |
| 7 | **ghcr.io 預先 build 的 image** | 別人現在要用得自己 build，這是採用門檻最大的一道牆 | 低 |
