# エージェント
- 作業開始前に `git pull origin main` で最新を取得する
- 調査など競合しないタスクは積極的にサブエージェントを使う
- /codex にレビューを依頼する（計画・コードレビュー・コミット前）
  - 客観的な視点で十分なコンテキストを渡すこと

# タスク管理
- Linear で管理（チーム: SEL）。`LINEAR_API_KEY` で API にアクセス
- タスクごとにブランチを切り PR を作る。main に直接 push しない
- タスクの記述はシンプルな箇条書きで

# CI
- push 後は `gh run list --limit 1` で確認
- in_progress なら `gh run watch <run-id> --exit-status` で待つ
- 失敗したら修正する。放置しない

# ドキュメント
- コードが真。腐敗するドキュメントは作らない
