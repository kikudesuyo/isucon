MySQL / memcached の初期設定
アプリケーションは MySQL と memcached を使用しています。以下の設定で起動済みです。

項目 値
MySQL ユーザー isuconp
MySQL パスワード isuconp
MySQL データベース名 isuconp
memcached アドレス localhost:11211
MySQLに接続するには以下のコマンドを使用します。

1
mysql -u isuconp -pisuconp isuconp
