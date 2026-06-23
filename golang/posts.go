package main

import (
	"context"
	"fmt"

	"github.com/jmoiron/sqlx"
)

func makePosts(ctx context.Context, results []Post, csrfToken string, allComments bool) ([]Post, error) {
	if len(results) == 0 {
		return nil, nil
	}

	// 投稿IDリストを作成
	postIDs := make([]int, len(results))
	postMap := make(map[int]*Post, len(results))
	for i := range results {
		postIDs[i] = results[i].ID
		results[i].CSRFToken = csrfToken
		postMap[results[i].ID] = &results[i]
	}

	// 投稿ユーザーをキャッシュ優先で一括取得
	userIDs := make([]int, 0, len(results))
	for _, p := range results {
		userIDs = append(userIDs, p.UserID)
	}
	userMap, err := getUsersByIDs(ctx, userIDs)
	if err != nil {
		return nil, fmt.Errorf("makePosts select users: %w", err)
	}

	counts, err := getCommentCountsByPostIDs(ctx, postIDs)
	if err != nil {
		return nil, fmt.Errorf("makePosts select comment counts: %w", err)
	}
	for postID, count := range counts {
		if p, ok := postMap[postID]; ok {
			p.CommentCount = count
		}
	}

	var allCommentsList []Comment
	if allComments {
		query, args, err := sqlx.In("SELECT * FROM `comments` WHERE `post_id` IN (?) ORDER BY `created_at` DESC", postIDs)
		if err != nil {
			return nil, fmt.Errorf("makePosts IN comments: %w", err)
		}
		if err = db.SelectContext(ctx, &allCommentsList, query, args...); err != nil {
			return nil, fmt.Errorf("makePosts select comments: %w", err)
		}
	} else {
		commentsByPost, err := getLatestCommentsByPostIDs(ctx, postIDs)
		if err != nil {
			return nil, fmt.Errorf("makePosts select latest comments: %w", err)
		}
		for _, comments := range commentsByPost {
			allCommentsList = append(allCommentsList, comments...)
		}
	}

	// コメントユーザーを一括取得
	commentUserIDSet := make(map[int]struct{})
	for _, c := range allCommentsList {
		commentUserIDSet[c.UserID] = struct{}{}
	}
	if len(commentUserIDSet) > 0 {
		commentUserIDs := make([]int, 0, len(commentUserIDSet))
		for id := range commentUserIDSet {
			commentUserIDs = append(commentUserIDs, id)
		}
		commentUsers, err := getUsersByIDs(ctx, commentUserIDs)
		if err != nil {
			return nil, fmt.Errorf("makePosts select comment users: %w", err)
		}
		for id, u := range commentUsers {
			userMap[id] = u
		}
	}

	// コメントを投稿ごとに振り分け（DESC順で取得済み → allComments=falseなら3件に絞り → 昇順に逆転）
	commentsByPost := make(map[int][]Comment)
	for _, c := range allCommentsList {
		c.User = userMap[c.UserID]
		if allComments || len(commentsByPost[c.PostID]) < 3 {
			commentsByPost[c.PostID] = append(commentsByPost[c.PostID], c)
		}
	}
	for postID, comments := range commentsByPost {
		for i, j := 0, len(comments)-1; i < j; i, j = i+1, j-1 {
			comments[i], comments[j] = comments[j], comments[i]
		}
		commentsByPost[postID] = comments
	}

	// 元の順序を保ってpostsPerPage件まで返す
	var posts []Post
	for i := range results {
		p := results[i]
		u, ok := userMap[p.UserID]
		if !ok || u.DelFlg != 0 {
			continue
		}
		p.User = u
		p.Comments = commentsByPost[p.ID]
		posts = append(posts, p)
		if len(posts) >= postsPerPage {
			break
		}
	}

	return posts, nil
}
