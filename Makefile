HOST := isucon@13.230.234.124
KEY  := $(dir $(abspath $(lastword $(MAKEFILE_LIST))))ws-default-keypair.pem
SSH  := ssh -i $(KEY) $(HOST)
SCP  := scp -i $(KEY)
REMOTE_DIR := ~/private_isu/webapp/golang

.PHONY: deploy build restart logs ssh add-index

# ビルド（ローカル）
build:
	cd golang && go build -o app .

# app.goをEC2に転送してビルド・再起動
deploy:
	$(SCP) golang/app.go $(HOST):$(REMOTE_DIR)/app.go
	$(SSH) "cd $(REMOTE_DIR) && PATH=/home/isucon/.local/go/bin:\$$PATH make app && sudo systemctl restart isu-go && echo '✅ デプロイ完了'"

# EC2でサービス再起動のみ
restart:
	$(SSH) "sudo systemctl restart isu-go && echo '✅ 再起動完了'"

# EC2のログをリアルタイム確認（context canceledは除外）
logs:
	$(SSH) "sudo journalctl -u isu-go -f | grep -v 'context canceled'"

# EC2にSSH接続
ssh:
	$(SSH)

# MySQLにインデックスを追加
add-index:
	$(SSH) "mysql -u isuconp -pisuconp isuconp -e '\
		ALTER TABLE posts ADD INDEX IF NOT EXISTS idx_created_at (created_at); \
		ALTER TABLE comments ADD INDEX IF NOT EXISTS idx_post_id (post_id); \
		ALTER TABLE comments ADD INDEX IF NOT EXISTS idx_user_id (user_id); \
	' && echo '✅ インデックス追加完了'"
