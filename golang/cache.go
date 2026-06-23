package main

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"sync"
	"time"

	"github.com/bradfitz/gomemcache/memcache"
	"github.com/jmoiron/sqlx"
)

var (
	memcacheClient *memcache.Client
	reAccountName  = regexp.MustCompile(`\A[0-9a-zA-Z_]{3,}\z`)
	rePassword     = regexp.MustCompile(`\A[0-9a-zA-Z_]{6,}\z`)
	cacheNamespace = strconv.FormatInt(time.Now().UnixNano(), 36)
	userCache      = struct {
		sync.RWMutex
		byID       map[int]User
		byAccount  map[string]User
		generation int64
	}{
		byID:       make(map[int]User),
		byAccount:  make(map[string]User),
		generation: 1,
	}
	commentCountCache = struct {
		sync.RWMutex
		byPostID   map[int]int
		generation int64
	}{
		byPostID:   make(map[int]int),
		generation: 1,
	}
	latestCommentsCache = struct {
		sync.RWMutex
		byPostID   map[int][]Comment
		generation int64
	}{
		byPostID:   make(map[int][]Comment),
		generation: 1,
	}
)

func clearUserCache() {
	userCache.Lock()
	userCache.byID = make(map[int]User)
	userCache.byAccount = make(map[string]User)
	userCache.generation++
	userCache.Unlock()
}

func setUserCache(u User) {
	setLocalUserCache(u)
	setMemcachedUser(u)
}

func setLocalUserCache(u User) {
	userCache.Lock()
	userCache.byID[u.ID] = u
	userCache.byAccount[u.AccountName] = u
	userCache.Unlock()
}

func deleteUserCacheByID(id int) {
	var cachedUser User
	hadCachedUser := false

	userCache.Lock()
	if u, ok := userCache.byID[id]; ok {
		delete(userCache.byAccount, u.AccountName)
		cachedUser = u
		hadCachedUser = true
	}
	delete(userCache.byID, id)
	userCache.Unlock()

	if hadCachedUser {
		deleteMemcachedUser(cachedUser)
	} else {
		deleteMemcachedUserByID(id)
	}
}

func getCachedUserByID(id int) (User, bool) {
	userCache.RLock()
	u, ok := userCache.byID[id]
	userCache.RUnlock()
	return u, ok
}

func getCachedUserByAccount(accountName string) (User, bool) {
	userCache.RLock()
	u, ok := userCache.byAccount[accountName]
	userCache.RUnlock()
	return u, ok
}

func userCacheGeneration() int64 {
	userCache.RLock()
	generation := userCache.generation
	userCache.RUnlock()
	return generation
}

func userCacheKeyByID(id int) string {
	return fmt.Sprintf("isuconp:%s:%d:user:id:%d", cacheNamespace, userCacheGeneration(), id)
}

func userCacheKeyByAccount(accountName string) string {
	return fmt.Sprintf("isuconp:%s:%d:user:account:%s", cacheNamespace, userCacheGeneration(), accountName)
}

func getMemcachedUser(key string) (User, bool) {
	item, err := memcacheClient.Get(key)
	if err != nil {
		return User{}, false
	}

	u := User{}
	if err := json.Unmarshal(item.Value, &u); err != nil {
		return User{}, false
	}
	return u, true
}

func setMemcachedUser(u User) {
	value, err := json.Marshal(u)
	if err != nil {
		return
	}
	_ = memcacheClient.Set(&memcache.Item{
		Key:   userCacheKeyByID(u.ID),
		Value: value,
	})
	_ = memcacheClient.Set(&memcache.Item{
		Key:   userCacheKeyByAccount(u.AccountName),
		Value: value,
	})
}

func deleteMemcachedUser(u User) {
	_ = memcacheClient.Delete(userCacheKeyByID(u.ID))
	_ = memcacheClient.Delete(userCacheKeyByAccount(u.AccountName))
}

func deleteMemcachedUserByID(id int) {
	_ = memcacheClient.Delete(userCacheKeyByID(id))
}

func getUserByID(ctx context.Context, id int) (User, error) {
	if u, ok := getCachedUserByID(id); ok {
		return u, nil
	}
	if u, ok := getMemcachedUser(userCacheKeyByID(id)); ok {
		setLocalUserCache(u)
		return u, nil
	}

	u := User{}
	if err := db.GetContext(ctx, &u, "SELECT * FROM `users` WHERE `id` = ?", id); err != nil {
		return User{}, err
	}
	setUserCache(u)
	return u, nil
}

func getActiveUserByAccount(ctx context.Context, accountName string) (User, error) {
	if u, ok := getCachedUserByAccount(accountName); ok && u.DelFlg == 0 {
		return u, nil
	}
	if u, ok := getMemcachedUser(userCacheKeyByAccount(accountName)); ok && u.DelFlg == 0 {
		setLocalUserCache(u)
		return u, nil
	}

	u := User{}
	if err := db.GetContext(ctx, &u, "SELECT * FROM users WHERE account_name = ? AND del_flg = 0", accountName); err != nil {
		return User{}, err
	}
	setUserCache(u)
	return u, nil
}

func getUsersByIDs(ctx context.Context, ids []int) (map[int]User, error) {
	users := make(map[int]User, len(ids))
	missingSet := make(map[int]struct{})
	memcacheKeyToID := make(map[string]int)
	memcacheKeys := make([]string, 0, len(ids))
	for _, id := range ids {
		if _, seen := users[id]; seen {
			continue
		}
		if u, ok := getCachedUserByID(id); ok {
			users[id] = u
		} else {
			key := userCacheKeyByID(id)
			memcacheKeyToID[key] = id
			memcacheKeys = append(memcacheKeys, key)
			missingSet[id] = struct{}{}
		}
	}

	if len(memcacheKeys) > 0 {
		items, err := memcacheClient.GetMulti(memcacheKeys)
		if err == nil {
			for key, item := range items {
				u := User{}
				if err := json.Unmarshal(item.Value, &u); err != nil {
					continue
				}
				setLocalUserCache(u)
				users[u.ID] = u
				delete(missingSet, memcacheKeyToID[key])
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

func clearCommentCountCache() {
	commentCountCache.Lock()
	commentCountCache.byPostID = make(map[int]int)
	commentCountCache.generation++
	commentCountCache.Unlock()
}

func commentCountCacheGeneration() int64 {
	commentCountCache.RLock()
	generation := commentCountCache.generation
	commentCountCache.RUnlock()
	return generation
}

func commentCountCacheKey(postID int) string {
	return fmt.Sprintf("isuconp:%s:%d:comment_count:post:%d", cacheNamespace, commentCountCacheGeneration(), postID)
}

func getCachedCommentCount(postID int) (int, bool) {
	commentCountCache.RLock()
	count, ok := commentCountCache.byPostID[postID]
	commentCountCache.RUnlock()
	return count, ok
}

func setLocalCommentCountCache(postID, count int) {
	commentCountCache.Lock()
	commentCountCache.byPostID[postID] = count
	commentCountCache.Unlock()
}

func setCommentCountCache(postID, count int) {
	setLocalCommentCountCache(postID, count)
	_ = memcacheClient.Set(&memcache.Item{
		Key:   commentCountCacheKey(postID),
		Value: []byte(strconv.Itoa(count)),
	})
}

func incrementCommentCountCache(postID int) {
	commentCountCache.Lock()
	if count, ok := commentCountCache.byPostID[postID]; ok {
		commentCountCache.byPostID[postID] = count + 1
	}
	commentCountCache.Unlock()
	_, _ = memcacheClient.Increment(commentCountCacheKey(postID), 1)
}

func getCommentCountsByPostIDs(ctx context.Context, postIDs []int) (map[int]int, error) {
	counts := make(map[int]int, len(postIDs))
	missingSet := make(map[int]struct{})
	memcacheKeyToPostID := make(map[string]int)
	memcacheKeys := make([]string, 0, len(postIDs))

	for _, postID := range postIDs {
		if _, seen := counts[postID]; seen {
			continue
		}
		if count, ok := getCachedCommentCount(postID); ok {
			counts[postID] = count
		} else {
			key := commentCountCacheKey(postID)
			memcacheKeyToPostID[key] = postID
			memcacheKeys = append(memcacheKeys, key)
			missingSet[postID] = struct{}{}
		}
	}

	if len(memcacheKeys) > 0 {
		items, err := memcacheClient.GetMulti(memcacheKeys)
		if err == nil {
			for key, item := range items {
				count, err := strconv.Atoi(string(item.Value))
				if err != nil {
					continue
				}
				postID := memcacheKeyToPostID[key]
				setLocalCommentCountCache(postID, count)
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

func clearLatestCommentsCache() {
	latestCommentsCache.Lock()
	latestCommentsCache.byPostID = make(map[int][]Comment)
	latestCommentsCache.generation++
	latestCommentsCache.Unlock()
}

func latestCommentsCacheGeneration() int64 {
	latestCommentsCache.RLock()
	generation := latestCommentsCache.generation
	latestCommentsCache.RUnlock()
	return generation
}

func latestCommentsCacheKey(postID int) string {
	return fmt.Sprintf("isuconp:%s:%d:latest_comments:post:%d", cacheNamespace, latestCommentsCacheGeneration(), postID)
}

func getCachedLatestComments(postID int) ([]Comment, bool) {
	latestCommentsCache.RLock()
	comments, ok := latestCommentsCache.byPostID[postID]
	latestCommentsCache.RUnlock()
	return comments, ok
}

func setLocalLatestCommentsCache(postID int, comments []Comment) {
	latestCommentsCache.Lock()
	latestCommentsCache.byPostID[postID] = comments
	latestCommentsCache.Unlock()
}

func setLatestCommentsCache(postID int, comments []Comment) {
	setLocalLatestCommentsCache(postID, comments)
	value, err := json.Marshal(comments)
	if err != nil {
		return
	}
	_ = memcacheClient.Set(&memcache.Item{
		Key:   latestCommentsCacheKey(postID),
		Value: value,
	})
}

func deleteLatestCommentsCache(postID int) {
	latestCommentsCache.Lock()
	delete(latestCommentsCache.byPostID, postID)
	latestCommentsCache.Unlock()
	_ = memcacheClient.Delete(latestCommentsCacheKey(postID))
}

func getLatestCommentsByPostIDs(ctx context.Context, postIDs []int) (map[int][]Comment, error) {
	commentsByPost := make(map[int][]Comment, len(postIDs))
	missingSet := make(map[int]struct{})
	memcacheKeyToPostID := make(map[string]int)
	memcacheKeys := make([]string, 0, len(postIDs))

	for _, postID := range postIDs {
		if _, seen := commentsByPost[postID]; seen {
			continue
		}
		if comments, ok := getCachedLatestComments(postID); ok {
			commentsByPost[postID] = comments
		} else {
			key := latestCommentsCacheKey(postID)
			memcacheKeyToPostID[key] = postID
			memcacheKeys = append(memcacheKeys, key)
			missingSet[postID] = struct{}{}
		}
	}

	if len(memcacheKeys) > 0 {
		items, err := memcacheClient.GetMulti(memcacheKeys)
		if err == nil {
			for key, item := range items {
				var comments []Comment
				if err := json.Unmarshal(item.Value, &comments); err != nil {
					continue
				}
				postID := memcacheKeyToPostID[key]
				setLocalLatestCommentsCache(postID, comments)
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
