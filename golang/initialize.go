package main

import "context"

func dbInitialize(ctx context.Context) {
	clearUserCache()
	clearCommentCountCache()
	clearLatestCommentsCache()
	cleanupGeneratedImageFiles()

	sqls := []string{
		"DELETE FROM users WHERE id > 1000",
		"DELETE FROM posts WHERE id > 10000",
		"DELETE FROM comments WHERE id > 100000",
		"UPDATE users SET del_flg = 0",
		"UPDATE users SET del_flg = 1 WHERE id % 50 = 0",
	}

	for _, sql := range sqls {
		db.ExecContext(ctx, sql)
	}
}
