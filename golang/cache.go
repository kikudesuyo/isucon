package main

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"sync"

	"github.com/bradfitz/gomemcache/memcache"
	"github.com/jmoiron/sqlx"
)

var (
	memcacheClient *memcache.Client
	reAccountName  = regexp.MustCompile(`\A[0-9a-zA-Z_]{3,}\z`)
	rePassword     = regexp.MustCompile(`\A[0-9a-zA-Z_]{6,}\z`)
	localCache     = struct {
		sync.RWMutex
		usersByID       map[int]User
		usersByAccount  map[string]User
		postsByID       map[int]Post
		commentCounts   map[int]int
		latestComments  map[int][]Comment
		indexPostIDs    []int
		hasIndexPostIDs bool
	}{
		usersByID:      make(map[int]User),
		usersByAccount: make(map[string]User),
		postsByID:      make(map[int]Post),
		commentCounts:  make(map[int]int),
		latestComments: make(map[int][]Comment),
	}
)

func clearLocalCache() {
	localCache.Lock()
	localCache.usersByID = make(map[int]User)
	localCache.usersByAccount = make(map[string]User)
	localCache.postsByID = make(map[int]Post)
	localCache.commentCounts = make(map[int]int)
	localCache.latestComments = make(map[int][]Comment)
	localCache.indexPostIDs = nil
	localCache.hasIndexPostIDs = false
	localCache.Unlock()
}

// --- User ---

func userKeyByID(id int) string {
	return fmt.Sprintf("user:id:%d", id)
}

func userKeyByAccount(accountName string) string {
	return fmt.Sprintf("user:account:%s", accountName)
}

func setUserCache(u User) {
	localCache.Lock()
	localCache.usersByID[u.ID] = u
	localCache.usersByAccount[u.AccountName] = u
	localCache.Unlock()

	value, err := json.Marshal(u)
	if err != nil {
		return
	}
	_ = memcacheClient.Set(&memcache.Item{Key: userKeyByID(u.ID), Value: value})
	_ = memcacheClient.Set(&memcache.Item{Key: userKeyByAccount(u.AccountName), Value: value})
}

func deleteUserCacheByID(id int) {
	localCache.Lock()
	if u, ok := localCache.usersByID[id]; ok {
		delete(localCache.usersByAccount, u.AccountName)
	}
	delete(localCache.usersByID, id)
	localCache.Unlock()

	key := userKeyByID(id)
	item, err := memcacheClient.Get(key)
	if err == nil {
		u := User{}
		if json.Unmarshal(item.Value, &u) == nil {
			_ = memcacheClient.Delete(userKeyByAccount(u.AccountName))
		}
	}
	_ = memcacheClient.Delete(key)
}

func getUserByID(ctx context.Context, id int) (User, error) {
	localCache.RLock()
	u, ok := localCache.usersByID[id]
	localCache.RUnlock()
	if ok {
		return u, nil
	}

	item, err := memcacheClient.Get(userKeyByID(id))
	if err == nil {
		u = User{}
		if json.Unmarshal(item.Value, &u) == nil {
			setUserCache(u)
			return u, nil
		}
	}

	u = User{}
	if err := db.GetContext(ctx, &u, "SELECT * FROM `users` WHERE `id` = ?", id); err != nil {
		return User{}, err
	}
	setUserCache(u)
	return u, nil
}

func getActiveUserByAccount(ctx context.Context, accountName string) (User, error) {
	localCache.RLock()
	u, ok := localCache.usersByAccount[accountName]
	localCache.RUnlock()
	if ok && u.DelFlg == 0 {
		return u, nil
	}

	item, err := memcacheClient.Get(userKeyByAccount(accountName))
	if err == nil {
		u = User{}
		if json.Unmarshal(item.Value, &u) == nil && u.DelFlg == 0 {
			setUserCache(u)
			return u, nil
		}
	}

	u = User{}
	if err := db.GetContext(ctx, &u, "SELECT * FROM users WHERE account_name = ? AND del_flg = 0", accountName); err != nil {
		return User{}, err
	}
	setUserCache(u)
	return u, nil
}

func getUsersByIDs(ctx context.Context, ids []int) (map[int]User, error) {
	users := make(map[int]User, len(ids))
	missingSet := make(map[int]struct{})
	keyToID := make(map[string]int)
	keys := make([]string, 0, len(ids))

	for _, id := range ids {
		if _, seen := users[id]; seen {
			continue
		}
		localCache.RLock()
		u, ok := localCache.usersByID[id]
		localCache.RUnlock()
		if ok {
			users[id] = u
			continue
		}
		key := userKeyByID(id)
		keyToID[key] = id
		keys = append(keys, key)
		missingSet[id] = struct{}{}
	}

	if len(keys) > 0 {
		items, err := memcacheClient.GetMulti(keys)
		if err == nil {
			for key, item := range items {
				u := User{}
				if json.Unmarshal(item.Value, &u) != nil {
					continue
				}
				setUserCache(u)
				users[u.ID] = u
				delete(missingSet, keyToID[key])
			}
		}
	}

	if len(missingSet) == 0 {
		return users, nil
	}

	missingIDs := make([]int, 0, len(missingSet))
	for id := range missingSet {
		missingIDs = append(missingIDs, id)
	}
	query, args, err := sqlx.In("SELECT * FROM `users` WHERE `id` IN (?)", missingIDs)
	if err != nil {
		return nil, err
	}
	var fetched []User
	if err := db.SelectContext(ctx, &fetched, query, args...); err != nil {
		return nil, err
	}
	for _, u := range fetched {
		setUserCache(u)
		users[u.ID] = u
	}
	return users, nil
}

// --- Post ---

func postKey(id int) string {
	return fmt.Sprintf("post:id:%d", id)
}

func setPostCache(p Post) {
	p.Imgdata = nil
	p.CommentCount = 0
	p.Comments = nil
	p.User = User{}
	p.CSRFToken = ""

	localCache.Lock()
	localCache.postsByID[p.ID] = p
	localCache.Unlock()

	value, err := json.Marshal(p)
	if err != nil {
		return
	}
	_ = memcacheClient.Set(&memcache.Item{Key: postKey(p.ID), Value: value})
}

func getPostByID(ctx context.Context, id int) (Post, error) {
	localCache.RLock()
	p, ok := localCache.postsByID[id]
	localCache.RUnlock()
	if ok {
		return p, nil
	}

	item, err := memcacheClient.Get(postKey(id))
	if err == nil {
		p = Post{}
		if json.Unmarshal(item.Value, &p) == nil {
			setPostCache(p)
			return p, nil
		}
	}

	p = Post{}
	if err := db.GetContext(ctx, &p, "SELECT `id`, `user_id`, `body`, `mime`, `created_at` FROM `posts` WHERE `id` = ?", id); err != nil {
		return Post{}, err
	}
	setPostCache(p)
	return p, nil
}

// --- Comment count ---

func commentCountKey(postID int) string {
	return fmt.Sprintf("comment_count:post:%d", postID)
}

func setCommentCountCache(postID, count int) {
	localCache.Lock()
	localCache.commentCounts[postID] = count
	localCache.Unlock()

	_ = memcacheClient.Set(&memcache.Item{
		Key:   commentCountKey(postID),
		Value: []byte(strconv.Itoa(count)),
	})
}

func incrementCommentCountCache(postID int) {
	localCache.Lock()
	if count, ok := localCache.commentCounts[postID]; ok {
		localCache.commentCounts[postID] = count + 1
	}
	localCache.Unlock()

	_, _ = memcacheClient.Increment(commentCountKey(postID), 1)
}

func getCommentCountsByPostIDs(ctx context.Context, postIDs []int) (map[int]int, error) {
	counts := make(map[int]int, len(postIDs))
	missingSet := make(map[int]struct{})
	keyToPostID := make(map[string]int)
	keys := make([]string, 0, len(postIDs))

	for _, postID := range postIDs {
		if _, seen := counts[postID]; seen {
			continue
		}
		localCache.RLock()
		count, ok := localCache.commentCounts[postID]
		localCache.RUnlock()
		if ok {
			counts[postID] = count
			continue
		}
		key := commentCountKey(postID)
		keyToPostID[key] = postID
		keys = append(keys, key)
		missingSet[postID] = struct{}{}
	}

	if len(keys) > 0 {
		items, err := memcacheClient.GetMulti(keys)
		if err == nil {
			for key, item := range items {
				count, err := strconv.Atoi(string(item.Value))
				if err != nil {
					continue
				}
				postID := keyToPostID[key]
				setCommentCountCache(postID, count)
				counts[postID] = count
				delete(missingSet, postID)
			}
		}
	}

	if len(missingSet) == 0 {
		return counts, nil
	}

	missingPostIDs := make([]int, 0, len(missingSet))
	for postID := range missingSet {
		missingPostIDs = append(missingPostIDs, postID)
	}

	type commentCount struct {
		PostID int `db:"post_id"`
		Count  int `db:"count"`
	}
	query, args, err := sqlx.In("SELECT `post_id`, COUNT(*) AS `count` FROM `comments` WHERE `post_id` IN (?) GROUP BY `post_id`", missingPostIDs)
	if err != nil {
		return nil, err
	}
	var fetched []commentCount
	if err := db.SelectContext(ctx, &fetched, query, args...); err != nil {
		return nil, err
	}
	for _, c := range fetched {
		counts[c.PostID] = c.Count
		delete(missingSet, c.PostID)
	}
	for postID := range missingSet {
		counts[postID] = 0
	}
	for postID, count := range counts {
		setCommentCountCache(postID, count)
	}
	return counts, nil
}

// --- Index post IDs ---

const indexPostsCacheKey = "index:post_ids"

func setIndexPostsCache(postIDs []int) {
	localCache.Lock()
	localCache.indexPostIDs = append(localCache.indexPostIDs[:0], postIDs...)
	localCache.hasIndexPostIDs = true
	localCache.Unlock()

	value, err := json.Marshal(postIDs)
	if err != nil {
		return
	}
	_ = memcacheClient.Set(&memcache.Item{Key: indexPostsCacheKey, Value: value})
}

func getIndexPostsCache() ([]int, bool) {
	localCache.RLock()
	if localCache.hasIndexPostIDs {
		postIDs := append([]int(nil), localCache.indexPostIDs...)
		localCache.RUnlock()
		return postIDs, true
	}
	localCache.RUnlock()

	item, err := memcacheClient.Get(indexPostsCacheKey)
	if err != nil {
		return nil, false
	}
	var postIDs []int
	if json.Unmarshal(item.Value, &postIDs) != nil {
		return nil, false
	}
	localCache.Lock()
	localCache.indexPostIDs = append(localCache.indexPostIDs[:0], postIDs...)
	localCache.hasIndexPostIDs = true
	localCache.Unlock()
	return postIDs, true
}

func deleteIndexPostsCache() {
	localCache.Lock()
	localCache.indexPostIDs = nil
	localCache.hasIndexPostIDs = false
	localCache.Unlock()
	_ = memcacheClient.Delete(indexPostsCacheKey)
}

// --- Latest comments ---

func latestCommentsKey(postID int) string {
	return fmt.Sprintf("latest_comments:post:%d", postID)
}

func setLatestCommentsCache(postID int, comments []Comment) {
	localCache.Lock()
	localCache.latestComments[postID] = comments
	localCache.Unlock()

	value, err := json.Marshal(comments)
	if err != nil {
		return
	}
	_ = memcacheClient.Set(&memcache.Item{
		Key:   latestCommentsKey(postID),
		Value: value,
	})
}

func deleteLatestCommentsCache(postID int) {
	localCache.Lock()
	delete(localCache.latestComments, postID)
	localCache.Unlock()
	_ = memcacheClient.Delete(latestCommentsKey(postID))
}

func getLatestCommentsByPostIDs(ctx context.Context, postIDs []int) (map[int][]Comment, error) {
	commentsByPost := make(map[int][]Comment, len(postIDs))
	missingSet := make(map[int]struct{})
	keyToPostID := make(map[string]int)
	keys := make([]string, 0, len(postIDs))

	for _, postID := range postIDs {
		if _, seen := commentsByPost[postID]; seen {
			continue
		}
		localCache.RLock()
		comments, ok := localCache.latestComments[postID]
		localCache.RUnlock()
		if ok {
			commentsByPost[postID] = comments
			continue
		}
		key := latestCommentsKey(postID)
		keyToPostID[key] = postID
		keys = append(keys, key)
		missingSet[postID] = struct{}{}
	}

	if len(keys) > 0 {
		items, err := memcacheClient.GetMulti(keys)
		if err == nil {
			for key, item := range items {
				var comments []Comment
				if json.Unmarshal(item.Value, &comments) != nil {
					continue
				}
				postID := keyToPostID[key]
				setLatestCommentsCache(postID, comments)
				commentsByPost[postID] = comments
				delete(missingSet, postID)
			}
		}
	}

	if len(missingSet) == 0 {
		return commentsByPost, nil
	}

	missingPostIDs := make([]int, 0, len(missingSet))
	for postID := range missingSet {
		missingPostIDs = append(missingPostIDs, postID)
	}
	query, args, err := sqlx.In(`
		SELECT id, post_id, user_id, comment, created_at
		FROM (
			SELECT id, post_id, user_id, comment, created_at,
				ROW_NUMBER() OVER (PARTITION BY post_id ORDER BY created_at DESC) AS rn
			FROM comments
			WHERE post_id IN (?)
		) AS ranked
		WHERE rn <= 3
		ORDER BY post_id, created_at DESC
	`, missingPostIDs)
	if err != nil {
		return nil, err
	}
	var fetched []Comment
	if err := db.SelectContext(ctx, &fetched, query, args...); err != nil {
		return nil, err
	}
	for _, c := range fetched {
		commentsByPost[c.PostID] = append(commentsByPost[c.PostID], c)
		delete(missingSet, c.PostID)
	}
	for postID := range missingSet {
		commentsByPost[postID] = nil
	}
	for postID, comments := range commentsByPost {
		setLatestCommentsCache(postID, comments)
	}
	return commentsByPost, nil
}
