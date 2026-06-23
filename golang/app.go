package main

import (
	"context"
	crand "crypto/rand"
	"crypto/sha512"
	"encoding/json"
	"fmt"
	"html/template"
	"io"
	"log"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/bradfitz/gomemcache/memcache"
	gsm "github.com/bradleypeabody/gorilla-sessions-memcache"
	"github.com/go-chi/chi/v5"
	mysql "github.com/go-sql-driver/mysql"
	"github.com/gorilla/sessions"
	"github.com/jmoiron/sqlx"
)

var (
	db    *sqlx.DB
	store *gsm.MemcacheStore
)

const (
	postsPerPage  = 20
	ISO8601Format = "2006-01-02T15:04:05-07:00"
	UploadLimit   = 10 * 1024 * 1024 // 10mb
)

var imageDir = "../public/image"

type User struct {
	ID          int       `db:"id"`
	AccountName string    `db:"account_name"`
	Passhash    string    `db:"passhash"`
	Authority   int       `db:"authority"`
	DelFlg      int       `db:"del_flg"`
	CreatedAt   time.Time `db:"created_at"`
}

type Post struct {
	ID           int       `db:"id"`
	UserID       int       `db:"user_id"`
	Imgdata      []byte    `db:"imgdata"`
	Body         string    `db:"body"`
	Mime         string    `db:"mime"`
	CreatedAt    time.Time `db:"created_at"`
	CommentCount int
	Comments     []Comment
	User         User
	CSRFToken    string
}

type Comment struct {
	ID        int       `db:"id"`
	PostID    int       `db:"post_id"`
	UserID    int       `db:"user_id"`
	Comment   string    `db:"comment"`
	CreatedAt time.Time `db:"created_at"`
	User      User
}

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

var templates map[string]*template.Template

func initTemplates() {
	fmap := template.FuncMap{"imageURL": imageURL}
	templates = map[string]*template.Template{
		"layout":   template.Must(template.New("layout.html").Funcs(fmap).ParseFiles(getTemplPath("layout.html"), getTemplPath("index.html"), getTemplPath("posts.html"), getTemplPath("post.html"))),
		"user":     template.Must(template.New("layout.html").Funcs(fmap).ParseFiles(getTemplPath("layout.html"), getTemplPath("user.html"), getTemplPath("posts.html"), getTemplPath("post.html"))),
		"posts":    template.Must(template.New("posts.html").Funcs(fmap).ParseFiles(getTemplPath("posts.html"), getTemplPath("post.html"))),
		"post_id":  template.Must(template.New("layout.html").Funcs(fmap).ParseFiles(getTemplPath("layout.html"), getTemplPath("post_id.html"), getTemplPath("post.html"))),
		"login":    template.Must(template.ParseFiles(getTemplPath("layout.html"), getTemplPath("login.html"))),
		"register": template.Must(template.ParseFiles(getTemplPath("layout.html"), getTemplPath("register.html"))),
		"banned":   template.Must(template.ParseFiles(getTemplPath("layout.html"), getTemplPath("banned.html"))),
	}
}

func init() {
	memdAddr := os.Getenv("ISUCONP_MEMCACHED_ADDRESS")
	if memdAddr == "" {
		memdAddr = "localhost:11211"
	}
	memcacheClient = memcache.New(memdAddr)
	store = gsm.NewMemcacheStore(memcacheClient, "iscogram_", []byte("sendagaya"))

	// JSON形式でstructuredログを出力
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{
		Level: slog.LevelDebug,
	})))
	log.SetFlags(0) // log.Fatalのフォーマットをslogに任せる
}

func dbInitialize(ctx context.Context) {
	clearUserCache()
	clearCommentCountCache()
	clearLatestCommentsCache()

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

func tryLogin(ctx context.Context, accountName, password string) *User {
	u, err := getActiveUserByAccount(ctx, accountName)
	if err != nil {
		return nil
	}

	if calculatePasshash(ctx, u.AccountName, password) == u.Passhash {
		return &u
	} else {
		return nil
	}
}

func validateUser(accountName, password string) bool {
	return reAccountName.MatchString(accountName) && rePassword.MatchString(password)
}

func digest(_ context.Context, src string) string {
	h := sha512.Sum512([]byte(src))
	return fmt.Sprintf("%x", h)
}

func calculateSalt(ctx context.Context, accountName string) string {
	return digest(ctx, accountName)
}

func calculatePasshash(ctx context.Context, accountName, password string) string {
	return digest(ctx, password+":"+calculateSalt(ctx, accountName))
}

func getSession(r *http.Request) *sessions.Session {
	session, _ := store.Get(r, "isuconp-go.session")

	return session
}

func getSessionUser(r *http.Request) User {
	ctx := r.Context()
	session := getSession(r)
	uid, ok := session.Values["user_id"]
	if !ok || uid == nil {
		return User{}
	}

	uidInt, ok := uid.(int)
	if !ok {
		uid64, ok := uid.(int64)
		if !ok {
			return User{}
		}
		uidInt = int(uid64)
	}

	u, err := getUserByID(ctx, uidInt)
	if err != nil {
		return User{}
	}

	return u
}

func getFlash(w http.ResponseWriter, r *http.Request, key string) string {
	session := getSession(r)
	value, ok := session.Values[key]

	if !ok || value == nil {
		return ""
	} else {
		delete(session.Values, key)
		session.Save(r, w)
		return value.(string)
	}
}

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

func imageURL(p Post) string {
	ext := ""
	if p.Mime == "image/jpeg" {
		ext = ".jpg"
	} else if p.Mime == "image/png" {
		ext = ".png"
	} else if p.Mime == "image/gif" {
		ext = ".gif"
	}

	return "/image/" + strconv.Itoa(p.ID) + ext
}

func imageExt(mime string) string {
	switch mime {
	case "image/jpeg":
		return ".jpg"
	case "image/png":
		return ".png"
	case "image/gif":
		return ".gif"
	default:
		return ""
	}
}

func imageFilePath(id int, mime string) string {
	return filepath.Join(imageDir, strconv.Itoa(id)+imageExt(mime))
}

func writeImageFile(id int, mime string, imgdata []byte) error {
	if imageExt(mime) == "" {
		return fmt.Errorf("unknown image mime: %s", mime)
	}
	if err := os.MkdirAll(imageDir, 0755); err != nil {
		return err
	}
	return os.WriteFile(imageFilePath(id, mime), imgdata, 0644)
}

func exportImages(ctx context.Context) error {
	if envImageDir := os.Getenv("ISUCONP_IMAGE_DIR"); envImageDir != "" {
		imageDir = envImageDir
	}
	if err := os.MkdirAll(imageDir, 0755); err != nil {
		return err
	}

	markerPath := filepath.Join(imageDir, ".exported")
	if _, err := os.Stat(markerPath); err == nil {
		return nil
	}

	rows, err := db.QueryxContext(ctx, "SELECT `id`, `mime`, `imgdata` FROM `posts`")
	if err != nil {
		return err
	}
	defer rows.Close()

	for rows.Next() {
		p := Post{}
		if err := rows.StructScan(&p); err != nil {
			return err
		}
		if err := writeImageFile(p.ID, p.Mime, p.Imgdata); err != nil {
			return err
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	return os.WriteFile(markerPath, []byte(time.Now().Format(time.RFC3339)), 0644)
}

func isLogin(u User) bool {
	return u.ID != 0
}

func getCSRFToken(r *http.Request) string {
	session := getSession(r)
	csrfToken, ok := session.Values["csrf_token"]
	if !ok {
		return ""
	}
	return csrfToken.(string)
}

func secureRandomStr(b int) string {
	k := make([]byte, b)
	if _, err := crand.Read(k); err != nil {
		panic(err)
	}
	return fmt.Sprintf("%x", k)
}

func getTemplPath(filename string) string {
	return path.Join("templates", filename)
}

func getInitialize(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	dbInitialize(ctx)
	w.WriteHeader(http.StatusOK)
}

func getLogin(w http.ResponseWriter, r *http.Request) {
	me := getSessionUser(r)

	if isLogin(me) {
		http.Redirect(w, r, "/", http.StatusFound)
		return
	}

	templates["login"].Execute(w, struct {
		Me    User
		Flash string
	}{me, getFlash(w, r, "notice")})
}

func postLogin(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	if isLogin(getSessionUser(r)) {
		http.Redirect(w, r, "/", http.StatusFound)
		return
	}

	u := tryLogin(ctx, r.FormValue("account_name"), r.FormValue("password"))

	if u != nil {
		session := getSession(r)
		session.Values["user_id"] = u.ID
		session.Values["csrf_token"] = secureRandomStr(16)
		session.Save(r, w)

		http.Redirect(w, r, "/", http.StatusFound)
	} else {
		session := getSession(r)
		session.Values["notice"] = "アカウント名かパスワードが間違っています"
		session.Save(r, w)

		http.Redirect(w, r, "/login", http.StatusFound)
	}
}

func getRegister(w http.ResponseWriter, r *http.Request) {
	if isLogin(getSessionUser(r)) {
		http.Redirect(w, r, "/", http.StatusFound)
		return
	}

	templates["register"].Execute(w, struct {
		Me    User
		Flash string
	}{User{}, getFlash(w, r, "notice")})
}

func postRegister(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	if isLogin(getSessionUser(r)) {
		http.Redirect(w, r, "/", http.StatusFound)
		return
	}

	accountName, password := r.FormValue("account_name"), r.FormValue("password")

	validated := validateUser(accountName, password)
	if !validated {
		session := getSession(r)
		session.Values["notice"] = "アカウント名は3文字以上、パスワードは6文字以上である必要があります"
		session.Save(r, w)

		http.Redirect(w, r, "/register", http.StatusFound)
		return
	}

	exists := 0
	// ユーザーが存在しない場合はエラーになるのでエラーチェックはしない
	db.GetContext(ctx, &exists, "SELECT 1 FROM users WHERE `account_name` = ?", accountName)

	if exists == 1 {
		session := getSession(r)
		session.Values["notice"] = "アカウント名がすでに使われています"
		session.Save(r, w)

		http.Redirect(w, r, "/register", http.StatusFound)
		return
	}

	query := "INSERT INTO `users` (`account_name`, `passhash`) VALUES (?,?)"
	result, err := db.ExecContext(ctx, query, accountName, calculatePasshash(ctx, accountName, password))
	if err != nil {
		log.Print(err)
		return
	}

	session := getSession(r)
	uid, err := result.LastInsertId()
	if err != nil {
		log.Print(err)
		return
	}
	session.Values["user_id"] = uid
	session.Values["csrf_token"] = secureRandomStr(16)
	session.Save(r, w)
	setUserCache(User{
		ID:          int(uid),
		AccountName: accountName,
		Passhash:    calculatePasshash(ctx, accountName, password),
	})

	http.Redirect(w, r, "/", http.StatusFound)
}

func getLogout(w http.ResponseWriter, r *http.Request) {
	session := getSession(r)
	delete(session.Values, "user_id")
	session.Options = &sessions.Options{MaxAge: -1}
	session.Save(r, w)

	http.Redirect(w, r, "/", http.StatusFound)
}

func getIndex(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	me := getSessionUser(r)

	results := []Post{}

	err := db.SelectContext(ctx, &results, "SELECT `id`, `user_id`, `body`, `mime`, `created_at` FROM `posts` ORDER BY `created_at` DESC LIMIT ?", postsPerPage*2)
	if err != nil {
		log.Print(err)
		return
	}

	posts, err := makePosts(ctx, results, getCSRFToken(r), false)
	if err != nil {
		log.Print(err)
		return
	}

	templates["layout"].Execute(w, struct {
		Posts     []Post
		Me        User
		CSRFToken string
		Flash     string
	}{posts, me, getCSRFToken(r), getFlash(w, r, "notice")})
}

func getAccountName(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	accountName := r.PathValue("accountName")

	user, err := getActiveUserByAccount(ctx, accountName)
	if err != nil {
		log.Print(err)
		return
	}

	if user.ID == 0 {
		w.WriteHeader(http.StatusNotFound)
		return
	}

	// posts・commentCount・postIDsを並列取得
	var (
		results      []Post
		posts        []Post
		commentCount int
		postIDs      []int
		errs         = make([]error, 3)
	)

	var wg sync.WaitGroup
	wg.Add(3)

	go func() {
		defer wg.Done()
		errs[0] = db.SelectContext(ctx, &results, "SELECT `id`, `user_id`, `body`, `mime`, `created_at` FROM `posts` WHERE `user_id` = ? ORDER BY `created_at` DESC", user.ID)
	}()
	go func() {
		defer wg.Done()
		errs[1] = db.GetContext(ctx, &commentCount, "SELECT COUNT(*) AS count FROM `comments` WHERE `user_id` = ?", user.ID)
	}()
	go func() {
		defer wg.Done()
		errs[2] = db.SelectContext(ctx, &postIDs, "SELECT `id` FROM `posts` WHERE `user_id` = ?", user.ID)
	}()
	wg.Wait()

	for _, err := range errs {
		if err != nil {
			log.Print(err)
			return
		}
	}

	posts, err = makePosts(ctx, results, getCSRFToken(r), false)
	if err != nil {
		log.Print(err)
		return
	}

	postCount := len(postIDs)
	commentedCount := 0
	if postCount > 0 {
		q, args, err := sqlx.In("SELECT COUNT(*) AS count FROM `comments` WHERE `post_id` IN (?)", postIDs)
		if err != nil {
			log.Print(err)
			return
		}
		if err = db.GetContext(ctx, &commentedCount, q, args...); err != nil {
			log.Print(err)
			return
		}
	}

	me := getSessionUser(r)

	templates["user"].Execute(w, struct {
		Posts          []Post
		User           User
		PostCount      int
		CommentCount   int
		CommentedCount int
		Me             User
	}{posts, user, postCount, commentCount, commentedCount, me})
}

func getPosts(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	m, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		log.Print(err)
		return
	}
	maxCreatedAt := m.Get("max_created_at")
	if maxCreatedAt == "" {
		return
	}

	t, err := time.Parse(ISO8601Format, maxCreatedAt)
	if err != nil {
		log.Print(err)
		return
	}

	results := []Post{}
	err = db.SelectContext(ctx, &results, "SELECT `id`, `user_id`, `body`, `mime`, `created_at` FROM `posts` WHERE `created_at` <= ? ORDER BY `created_at` DESC LIMIT ?", t.Format(ISO8601Format), postsPerPage*2)
	if err != nil {
		log.Print(err)
		return
	}

	posts, err := makePosts(ctx, results, getCSRFToken(r), false)
	if err != nil {
		log.Print(err)
		return
	}

	if len(posts) == 0 {
		w.WriteHeader(http.StatusNotFound)
		return
	}

	templates["posts"].Execute(w, posts)
}

func getPostsID(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	pidStr := r.PathValue("id")
	pid, err := strconv.Atoi(pidStr)
	if err != nil {
		w.WriteHeader(http.StatusNotFound)
		return
	}

	results := []Post{}
	err = db.SelectContext(ctx, &results, "SELECT `id`, `user_id`, `body`, `mime`, `created_at` FROM `posts` WHERE `id` = ?", pid)
	if err != nil {
		log.Print(err)
		return
	}

	posts, err := makePosts(ctx, results, getCSRFToken(r), true)
	if err != nil {
		log.Print(err)
		return
	}

	if len(posts) == 0 {
		w.WriteHeader(http.StatusNotFound)
		return
	}

	p := posts[0]

	me := getSessionUser(r)

	templates["post_id"].Execute(w, struct {
		Post Post
		Me   User
	}{p, me})
}

func postIndex(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	me := getSessionUser(r)
	if !isLogin(me) {
		http.Redirect(w, r, "/login", http.StatusFound)
		return
	}

	if r.FormValue("csrf_token") != getCSRFToken(r) {
		w.WriteHeader(http.StatusUnprocessableEntity)
		return
	}

	file, header, err := r.FormFile("file")
	if err != nil {
		session := getSession(r)
		session.Values["notice"] = "画像が必須です"
		session.Save(r, w)

		http.Redirect(w, r, "/", http.StatusFound)
		return
	}

	mime := ""
	if file != nil {
		// 投稿のContent-Typeからファイルのタイプを決定する
		contentType := header.Header["Content-Type"][0]
		if strings.Contains(contentType, "jpeg") {
			mime = "image/jpeg"
		} else if strings.Contains(contentType, "png") {
			mime = "image/png"
		} else if strings.Contains(contentType, "gif") {
			mime = "image/gif"
		} else {
			session := getSession(r)
			session.Values["notice"] = "投稿できる画像形式はjpgとpngとgifだけです"
			session.Save(r, w)

			http.Redirect(w, r, "/", http.StatusFound)
			return
		}
	}

	filedata, err := io.ReadAll(file)
	if err != nil {
		log.Print(err)
		return
	}

	if len(filedata) > UploadLimit {
		session := getSession(r)
		session.Values["notice"] = "ファイルサイズが大きすぎます"
		session.Save(r, w)

		http.Redirect(w, r, "/", http.StatusFound)
		return
	}

	query := "INSERT INTO `posts` (`user_id`, `mime`, `imgdata`, `body`) VALUES (?,?,?,?)"
	result, err := db.ExecContext(
		ctx,
		query,
		me.ID,
		mime,
		filedata,
		r.FormValue("body"),
	)
	if err != nil {
		log.Print(err)
		return
	}

	pid, err := result.LastInsertId()
	if err != nil {
		log.Print(err)
		return
	}
	if err := writeImageFile(int(pid), mime, filedata); err != nil {
		log.Print(err)
		return
	}

	http.Redirect(w, r, "/posts/"+strconv.FormatInt(pid, 10), http.StatusFound)
}

func getImage(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	pidStr := r.PathValue("id")
	pid, err := strconv.Atoi(pidStr)
	if err != nil {
		w.WriteHeader(http.StatusNotFound)
		return
	}

	post := Post{}
	err = db.GetContext(ctx, &post, "SELECT * FROM `posts` WHERE `id` = ?", pid)
	if err != nil {
		log.Print(err)
		return
	}

	ext := r.PathValue("ext")

	if ext == "jpg" && post.Mime == "image/jpeg" ||
		ext == "png" && post.Mime == "image/png" ||
		ext == "gif" && post.Mime == "image/gif" {
		w.Header().Set("Content-Type", post.Mime)
		_, err := w.Write(post.Imgdata)
		if err != nil {
			log.Print(err)
			return
		}
		return
	}

	w.WriteHeader(http.StatusNotFound)
}

func postComment(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	me := getSessionUser(r)
	if !isLogin(me) {
		http.Redirect(w, r, "/login", http.StatusFound)
		return
	}

	if r.FormValue("csrf_token") != getCSRFToken(r) {
		w.WriteHeader(http.StatusUnprocessableEntity)
		return
	}

	postID, err := strconv.Atoi(r.FormValue("post_id"))
	if err != nil {
		log.Print("post_idは整数のみです")
		return
	}

	query := "INSERT INTO `comments` (`post_id`, `user_id`, `comment`) VALUES (?,?,?)"
	_, err = db.ExecContext(ctx, query, postID, me.ID, r.FormValue("comment"))
	if err != nil {
		log.Print(err)
		return
	}
	incrementCommentCountCache(postID)
	deleteLatestCommentsCache(postID)

	http.Redirect(w, r, fmt.Sprintf("/posts/%d", postID), http.StatusFound)
}

func getAdminBanned(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	me := getSessionUser(r)
	if !isLogin(me) {
		http.Redirect(w, r, "/", http.StatusFound)
		return
	}

	if me.Authority == 0 {
		w.WriteHeader(http.StatusForbidden)
		return
	}

	users := []User{}
	err := db.SelectContext(ctx, &users, "SELECT * FROM `users` WHERE `authority` = 0 AND `del_flg` = 0 ORDER BY `created_at` DESC")
	if err != nil {
		log.Print(err)
		return
	}

	templates["banned"].Execute(w, struct {
		Users     []User
		Me        User
		CSRFToken string
	}{users, me, getCSRFToken(r)})
}

func postAdminBanned(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	me := getSessionUser(r)
	if !isLogin(me) {
		http.Redirect(w, r, "/", http.StatusFound)
		return
	}

	if me.Authority == 0 {
		w.WriteHeader(http.StatusForbidden)
		return
	}

	if r.FormValue("csrf_token") != getCSRFToken(r) {
		w.WriteHeader(http.StatusUnprocessableEntity)
		return
	}

	query := "UPDATE `users` SET `del_flg` = ? WHERE `id` = ?"

	err := r.ParseForm()
	if err != nil {
		log.Print(err)
		return
	}

	for _, id := range r.Form["uid[]"] {
		if _, err := db.ExecContext(ctx, query, 1, id); err != nil {
			log.Print(err)
			return
		}
		uid, err := strconv.Atoi(id)
		if err == nil {
			deleteUserCacheByID(uid)
		}
	}

	http.Redirect(w, r, "/admin/banned", http.StatusFound)
}

func main() {
	host := os.Getenv("ISUCONP_DB_HOST")
	if host == "" {
		host = "localhost"
	}
	port := os.Getenv("ISUCONP_DB_PORT")
	if port == "" {
		port = "3306"
	}
	_, err := strconv.Atoi(port)
	if err != nil {
		log.Fatalf("Failed to read DB port number from an environment variable ISUCONP_DB_PORT.\nError: %s", err.Error())
	}
	user := os.Getenv("ISUCONP_DB_USER")
	if user == "" {
		user = "root"
	}
	password := os.Getenv("ISUCONP_DB_PASSWORD")
	dbname := os.Getenv("ISUCONP_DB_NAME")
	if dbname == "" {
		dbname = "isuconp"
	}

	cfg := mysql.NewConfig()
	cfg.User = user
	cfg.Passwd = password
	cfg.Net = "tcp"
	cfg.Addr = fmt.Sprintf("%s:%s", host, port)
	cfg.DBName = dbname
	cfg.Params = map[string]string{
		"charset": "utf8mb4",
	}
	cfg.ParseTime = true
	cfg.Loc = time.Local
	dsn := cfg.FormatDSN()

	db, err = sqlx.Open("mysql", dsn)
	if err != nil {
		log.Fatalf("Failed to connect to DB: %s.", err.Error())
	}
	defer db.Close()

	db.SetMaxOpenConns(50)
	db.SetMaxIdleConns(50)
	db.SetConnMaxLifetime(5 * time.Minute)

	if err := exportImages(context.Background()); err != nil {
		log.Fatalf("Failed to export images: %s.", err.Error())
	}

	initTemplates()

	r := chi.NewRouter()

	r.Get("/initialize", getInitialize)
	r.Get("/login", getLogin)
	r.Post("/login", postLogin)
	r.Get("/register", getRegister)
	r.Post("/register", postRegister)
	r.Get("/logout", getLogout)
	r.Get("/", getIndex)
	r.Get("/posts", getPosts)
	r.Get("/posts/{id}", getPostsID)
	r.Post("/", postIndex)
	r.Get("/image/{id}.{ext}", getImage)
	r.Post("/comment", postComment)
	r.Get("/admin/banned", getAdminBanned)
	r.Post("/admin/banned", postAdminBanned)
	r.Get(`/@{accountName:[0-9a-zA-Z_]+}`, getAccountName)
	r.Mount("/", http.FileServer(http.Dir("../public")))

	log.Fatal(http.ListenAndServe(":8080", r))
}
