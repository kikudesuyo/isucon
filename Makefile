HOST := isucon@13.230.234.124
KEY  := $(dir $(abspath $(lastword $(MAKEFILE_LIST))))ws-default-keypair.pem
SSH  := ssh -i $(KEY) $(HOST)
SCP  := scp -i $(KEY)
REMOTE_DIR := ~/private_isu/webapp/golang

# 使い方: make pr ISSUE=5
# PRを作成してissueを紐づけ、マージ後にブランチとissueをclose
ISSUE ?=
BRANCH := $(shell git rev-parse --abbrev-ref HEAD)

NGINX_CONF := /etc/nginx/conf.d/isucon.conf

.PHONY: deploy build restart logs ssh add-index pr nginx-deploy nginx-reload

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

# PRを作成してissueに紐づけ、マージ後にブランチとissueをclose
# 使い方: make pr ISSUE=5
pr:
	@if [ -z "$(ISSUE)" ]; then echo "❌ ISSUE番号を指定してください: make pr ISSUE=5"; exit 1; fi
	git push -u origin $(BRANCH)
	gh pr create \
		--title "$(BRANCH)" \
		--body "closes #$(ISSUE)" \
		--base main \
		--head $(BRANCH)
	gh pr merge --squash --delete-branch --auto
	@echo "✅ PR作成完了 (issue #$(ISSUE) は自動でcloseされます)"

# nginx設定をEC2に転送してリロード
nginx-deploy:
	$(SCP) etc/nginx/isucon.conf $(HOST):$(NGINX_CONF)
	$(SSH) "sudo nginx -t && sudo systemctl reload nginx && echo '✅ nginx設定更新完了'"

# nginxをリロードのみ
nginx-reload:
	$(SSH) "sudo nginx -t && sudo systemctl reload nginx && echo '✅ nginxリロード完了'"

# MySQLにインデックスを追加(issue/4で対応)
add-index:
	$(SSH) "mysql -u isuconp -pisuconp isuconp -e '\
			SET @sql := IF((SELECT COUNT(*) FROM information_schema.statistics WHERE table_schema = DATABASE() AND table_name = \"posts\" AND index_name = \"idx_created_at\") = 0, \"ALTER TABLE posts ADD INDEX idx_created_at (created_at)\", \"SELECT 1\"); PREPARE stmt FROM @sql; EXECUTE stmt; DEALLOCATE PREPARE stmt; \
			SET @sql := IF((SELECT COUNT(*) FROM information_schema.statistics WHERE table_schema = DATABASE() AND table_name = \"comments\" AND index_name = \"idx_post_id\") = 0, \"ALTER TABLE comments ADD INDEX idx_post_id (post_id)\", \"SELECT 1\"); PREPARE stmt FROM @sql; EXECUTE stmt; DEALLOCATE PREPARE stmt; \
			SET @sql := IF((SELECT COUNT(*) FROM information_schema.statistics WHERE table_schema = DATABASE() AND table_name = \"comments\" AND index_name = \"idx_user_id\") = 0, \"ALTER TABLE comments ADD INDEX idx_user_id (user_id)\", \"SELECT 1\"); PREPARE stmt FROM @sql; EXECUTE stmt; DEALLOCATE PREPARE stmt; \
			SET @sql := IF((SELECT COUNT(*) FROM information_schema.statistics WHERE table_schema = DATABASE() AND table_name = \"users\" AND column_name = \"account_name\") = 0, \"ALTER TABLE users ADD INDEX idx_account_name (account_name)\", \"SELECT 1\"); PREPARE stmt FROM @sql; EXECUTE stmt; DEALLOCATE PREPARE stmt; \
		' && echo '✅ インデックス追加完了'"
