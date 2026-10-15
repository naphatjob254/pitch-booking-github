package main

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"log"
	"net"
	"net/http"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/gorilla/sessions"
	"golang.org/x/crypto/bcrypt"
)

var (
	store     *sessions.CookieStore
	tmpl      *template.Template
	dummyHash []byte // ใช้เทียบรหัสผ่านตอนไม่พบ user เพื่อให้เวลาตอบใกล้เคียงกัน
)

// ==========================================
// Structs
// ==========================================
type Viewer struct {
	ID       int
	Username string
	Role     string
}

// ==========================================
// Config / init
// ==========================================
func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func initStore() {
	key := os.Getenv("SESSION_KEY")
	if len(key) < 32 {
		b := make([]byte, 32)
		if _, err := rand.Read(b); err != nil {
			log.Fatal(err)
		}
		key = string(b)
		log.Println("⚠️  ไม่ได้ตั้ง SESSION_KEY (ยาว >= 32 ตัวอักษร) จึงสุ่มคีย์ชั่วคราว: ทุกครั้งที่รีสตาร์ท ผู้ใช้จะต้องล็อกอินใหม่")
	}
	enc := sha256.Sum256([]byte("enc:" + key))
	store = sessions.NewCookieStore([]byte(key), enc[:])
	store.Options = &sessions.Options{
		Path:     "/",
		MaxAge:   8 * 3600,
		HttpOnly: true,
		Secure:   os.Getenv("COOKIE_SECURE") == "1",
		SameSite: http.SameSiteLaxMode,
	}
}

// ==========================================
// Helpers: session / CSRF / render
// ==========================================
func newToken() string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

func getSession(r *http.Request) *sessions.Session {
	s, _ := store.Get(r, "session") // ถ้า cookie เสีย จะได้ session ใหม่กลับมา
	return s
}

func viewer(r *http.Request) (Viewer, bool) {
	s := getSession(r)
	id, ok := s.Values["user_id"].(int)
	if !ok || id <= 0 {
		return Viewer{}, false
	}
	name, _ := s.Values["username"].(string)
	role, _ := s.Values["role"].(string)
	return Viewer{ID: id, Username: name, Role: role}, true
}

func csrfToken(w http.ResponseWriter, r *http.Request) string {
	s := getSession(r)
	tok, _ := s.Values["csrf"].(string)
	if tok == "" {
		tok = newToken()
		s.Values["csrf"] = tok
		if err := s.Save(r, w); err != nil {
			log.Println("session save error:", err)
		}
	}
	return tok
}

func csrfOK(r *http.Request) bool {
	s := getSession(r)
	want, _ := s.Values["csrf"].(string)
	got := r.PostFormValue("csrf_token")
	return want != "" && subtle.ConstantTimeCompare([]byte(want), []byte(got)) == 1
}

func serverError(w http.ResponseWriter, err error) {
	log.Println("ERROR:", err)
	http.Error(w, "เกิดข้อผิดพลาดภายในระบบ", http.StatusInternalServerError)
}

func render(w http.ResponseWriter, r *http.Request, name string, data map[string]interface{}) {
	if data == nil {
		data = map[string]interface{}{}
	}
	data["CSRF"] = csrfToken(w, r) // ต้องเรียกก่อนเริ่มเขียน response
	if _, ok := data["Active"]; !ok {
		data["Active"] = ""
	}
	data["Role"] = ""
	if v, ok := viewer(r); ok {
		data["Username"] = v.Username
		data["UserID"] = v.ID
		data["Role"] = v.Role
	}
	var buf bytes.Buffer
	if err := tmpl.ExecuteTemplate(&buf, name, data); err != nil {
		serverError(w, err)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	buf.WriteTo(w)
}

// ==========================================
// Middleware
// ==========================================
type userHandler func(http.ResponseWriter, *http.Request, Viewer)

func auth(next userHandler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		v, ok := viewer(r)
		if !ok {
			http.Redirect(w, r, "/login", http.StatusSeeOther)
			return
		}
		next(w, r, v)
	}
}

func adminOnly(next userHandler) http.HandlerFunc {
	return auth(func(w http.ResponseWriter, r *http.Request, v Viewer) {
		if v.Role != "admin" {
			http.Error(w, "403 Forbidden: เฉพาะผู้ดูแลระบบ", http.StatusForbidden)
			return
		}
		next(w, r, v)
	})
}

// postOnly: อนุญาตเฉพาะ POST และต้องมี CSRF token ถูกต้อง
func postOnly(next userHandler) userHandler {
	return func(w http.ResponseWriter, r *http.Request, v Viewer) {
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", "POST")
			http.Error(w, "405 Method Not Allowed", http.StatusMethodNotAllowed)
			return
		}
		if !csrfOK(r) {
			http.Error(w, "403 CSRF token ไม่ถูกต้อง", http.StatusForbidden)
			return
		}
		next(w, r, v)
	}
}

func secure(next http.Handler) http.Handler {
	csp := "default-src 'self'; " +
		"script-src 'self' https://cdn.tailwindcss.com; " +
		"style-src 'self' 'unsafe-inline' https://fonts.googleapis.com; " +
		"font-src https://fonts.gstatic.com; " +
		"img-src 'self' data: https://images.unsplash.com; " +
		"form-action 'self'; frame-ancestors 'none'; base-uri 'self'; object-src 'none'"
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", csp)
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "same-origin")
		r.Body = http.MaxBytesReader(w, r.Body, 1<<20) // จำกัด body 1MB
		next.ServeHTTP(w, r)
	})
}

// ==========================================
// Rate limit การล็อกอิน (5 ครั้งผิด / 10 นาที ต่อ IP+username)
// ==========================================
type attempt struct {
	n     int
	first time.Time
}

var (
	limMu sync.Mutex
	fails = map[string]*attempt{}
)

const (
	maxFails   = 5
	failWindow = 10 * time.Minute
)

func clientIP(r *http.Request) string {
	if os.Getenv("TRUST_PROXY") == "1" { // เปิดเฉพาะตอนอยู่หลัง Cloudflare/ngrok
		if ip := r.Header.Get("CF-Connecting-IP"); ip != "" {
			return ip
		}
		if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
			return strings.TrimSpace(strings.Split(xff, ",")[0])
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

func isBlocked(key string) bool {
	limMu.Lock()
	defer limMu.Unlock()
	a := fails[key]
	if a == nil {
		return false
	}
	if time.Since(a.first) > failWindow {
		delete(fails, key)
		return false
	}
	return a.n >= maxFails
}

func recordFail(key string) {
	limMu.Lock()
	defer limMu.Unlock()
	if len(fails) > 5000 {
		for k, a := range fails {
			if time.Since(a.first) > failWindow {
				delete(fails, k)
			}
		}
	}
	a := fails[key]
	if a == nil || time.Since(a.first) > failWindow {
		fails[key] = &attempt{n: 1, first: time.Now()}
		return
	}
	a.n++
}

func clearFails(key string) {
	limMu.Lock()
	delete(fails, key)
	limMu.Unlock()
}

// ==========================================
// Validation
// ==========================================
var (
	usernameRe = regexp.MustCompile(`^[A-Za-z0-9_\x{0E00}-\x{0E7F}]{3,30}$`)
	phoneRe    = regexp.MustCompile(`^0\d{8,9}$`)
)

func cleanPhone(s string) string {
	return strings.NewReplacer("-", "", " ", "").Replace(strings.TrimSpace(s))
}

func checkPassword(p string) string {
	if utf8.RuneCountInString(p) < 8 {
		return "รหัสผ่านต้องยาวอย่างน้อย 8 ตัวอักษร"
	}
	if len(p) > 72 {
		return "รหัสผ่านยาวเกินไป (สูงสุด 72 ไบต์)"
	}
	return ""
}

// ==========================================
// main
// ==========================================
type noListFS struct{ http.FileSystem }

func (n noListFS) Open(path string) (http.File, error) {
	f, err := n.FileSystem.Open(path)
	if err != nil {
		return nil, err
	}
	st, err := f.Stat()
	if err != nil || st.IsDir() { // ปิดการแสดงรายการไฟล์ในโฟลเดอร์
		f.Close()
		return nil, os.ErrNotExist
	}
	return f, nil
}

func main() {
	initDB()
	defer db.Close()

	if len(os.Args) > 1 && os.Args[1] == "create-admin" {
		createAdmin(os.Args[2:])
		return
	}

	initStore()
	tmpl = loadTemplates()
	dummyHash, _ = bcrypt.GenerateFromPassword([]byte("dummy-password"), bcrypt.DefaultCost)

	mux := http.NewServeMux()
	mux.Handle("/static/", http.StripPrefix("/static/", http.FileServer(noListFS{staticFS()})))

	mux.HandleFunc("/register", registerHandler)
	mux.HandleFunc("/login", loginHandler)
	mux.HandleFunc("/logout", logoutHandler)

	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		auth(dashboardHandler)(w, r)
	})
	mux.HandleFunc("/match/create", auth(createMatchHandler))
	mux.HandleFunc("/match/edit", auth(editMatchHandler))
	mux.HandleFunc("/match/delete", auth(postOnly(deleteMatchHandler)))
	mux.HandleFunc("/match/join", auth(postOnly(joinMatchHandler)))

	mux.HandleFunc("/pitches", auth(pitchesHandler))
	mux.HandleFunc("/book", auth(bookHandler))
	mux.HandleFunc("/my-bookings", auth(myBookingsHandler))
	mux.HandleFunc("/booking/cancel", auth(postOnly(cancelBookingHandler)))

	mux.HandleFunc("/reviews", auth(reviewsHandler))
	mux.HandleFunc("/review/delete", auth(postOnly(deleteReviewHandler)))

	mux.HandleFunc("/admin", adminOnly(adminHandler))
	mux.HandleFunc("/admin/delete-review", adminOnly(postOnly(adminDeleteReviewHandler)))
	mux.HandleFunc("/admin/delete-match", adminOnly(postOnly(adminDeleteMatchHandler)))
	mux.HandleFunc("/admin/approve-booking", adminOnly(postOnly(adminApproveBookingHandler)))
	mux.HandleFunc("/admin/delete-booking", adminOnly(postOnly(adminDeleteBookingHandler)))

	mux.HandleFunc("/api/matches", auth(apiGetMatchesHandler))

	srv := &http.Server{
		Addr:              ":" + env("PORT", "8080"),
		Handler:           secure(mux),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      20 * time.Second,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    1 << 16,
	}
	log.Println("Server running on http://localhost" + srv.Addr)
	log.Fatal(srv.ListenAndServe())
}

// go run . create-admin <username> <phone> <password>
func createAdmin(args []string) {
	if len(args) != 3 {
		log.Fatal("วิธีใช้: go run . create-admin <username> <phone> <password>")
	}
	username, phone, password := args[0], cleanPhone(args[1]), args[2]
	if !usernameRe.MatchString(username) || !phoneRe.MatchString(phone) {
		log.Fatal("username (3-30 ตัว: ก-ฮ a-z 0-9 _) หรือเบอร์โทรไม่ถูกต้อง")
	}
	if msg := checkPassword(password); msg != "" {
		log.Fatal(msg)
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		log.Fatal(err)
	}
	_, err = db.Exec(`INSERT INTO users (username, phone, password, role) VALUES (?, ?, ?, 'admin')
		ON DUPLICATE KEY UPDATE password = VALUES(password), role = 'admin'`, username, phone, string(hash))
	if err != nil {
		log.Fatal(err)
	}
	log.Println("✅ สร้าง/อัปเดตแอดมินเรียบร้อย:", username)
}

// ==========================================
// Auth handlers
// ==========================================
func registerHandler(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		render(w, r, "register.html", nil)
	case http.MethodPost:
		if !csrfOK(r) {
			http.Error(w, "403 CSRF token ไม่ถูกต้อง", http.StatusForbidden)
			return
		}
		username := strings.TrimSpace(r.PostFormValue("username"))
		phone := cleanPhone(r.PostFormValue("phone"))
		password := r.PostFormValue("password")

		msg := ""
		switch {
		case !usernameRe.MatchString(username):
			msg = "ชื่อผู้ใช้ต้องยาว 3-30 ตัว ใช้ได้เฉพาะ ก-ฮ, a-z, 0-9 และ _"
		case !phoneRe.MatchString(phone):
			msg = "เบอร์โทรศัพท์ไม่ถูกต้อง (เช่น 0812345678)"
		default:
			msg = checkPassword(password)
		}
		if msg != "" {
			render(w, r, "register.html", map[string]interface{}{"Error": msg, "FormUsername": username, "FormPhone": phone})
			return
		}

		hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
		if err != nil {
			serverError(w, err)
			return
		}
		_, err = db.Exec("INSERT INTO users (username, phone, password, role) VALUES (?, ?, ?, 'user')", username, phone, string(hash))
		if err != nil {
			if isDup(err) {
				render(w, r, "register.html", map[string]interface{}{"Error": "ชื่อผู้ใช้นี้มีคนใช้งานแล้ว", "FormUsername": username, "FormPhone": phone})
				return
			}
			serverError(w, err)
			return
		}
		http.Redirect(w, r, "/login", http.StatusSeeOther)
	default:
		http.Error(w, "405 Method Not Allowed", http.StatusMethodNotAllowed)
	}
}

func loginHandler(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		render(w, r, "login.html", nil)
	case http.MethodPost:
		if !csrfOK(r) {
			http.Error(w, "403 CSRF token ไม่ถูกต้อง", http.StatusForbidden)
			return
		}
		username := strings.TrimSpace(r.PostFormValue("username"))
		password := r.PostFormValue("password")
		key := clientIP(r) + "|" + strings.ToLower(username)

		if isBlocked(key) {
			render(w, r, "login.html", map[string]interface{}{"Error": "ลองผิดหลายครั้งเกินไป กรุณารอ 10 นาทีแล้วลองใหม่"})
			return
		}

		var id int
		var uname, hash, role string
		err := db.QueryRow("SELECT id, username, password, role FROM users WHERE username = ?", username).Scan(&id, &uname, &hash, &role)
		found := err == nil
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			serverError(w, err)
			return
		}
		if !found {
			hash = string(dummyHash)
		}
		pwOK := bcrypt.CompareHashAndPassword([]byte(hash), []byte(password)) == nil
		if !found || !pwOK {
			recordFail(key)
			render(w, r, "login.html", map[string]interface{}{"Error": "ชื่อผู้ใช้หรือรหัสผ่านไม่ถูกต้อง"})
			return
		}
		clearFails(key)

		s := getSession(r)
		s.Values = map[interface{}]interface{}{} // ล้างค่าเดิม + ออก CSRF token ใหม่ (กัน session fixation)
		s.Values["user_id"] = id
		s.Values["username"] = uname
		s.Values["role"] = role
		s.Values["csrf"] = newToken()
		if err := s.Save(r, w); err != nil {
			serverError(w, err)
			return
		}
		if role == "admin" {
			http.Redirect(w, r, "/admin", http.StatusSeeOther)
			return
		}
		http.Redirect(w, r, "/", http.StatusSeeOther)
	default:
		http.Error(w, "405 Method Not Allowed", http.StatusMethodNotAllowed)
	}
}

func logoutHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	if !csrfOK(r) {
		http.Error(w, "403 CSRF token ไม่ถูกต้อง", http.StatusForbidden)
		return
	}
	s := getSession(r)
	s.Values = map[interface{}]interface{}{}
	s.Options.MaxAge = -1
	s.Save(r, w)
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}

// ==========================================
// Matches (CRUD ที่มีเจ้าของ)
// ==========================================
func dashboardHandler(w http.ResponseWriter, r *http.Request, v Viewer) {
	matches, err := loadMatches(v.ID)
	if err != nil {
		serverError(w, err)
		return
	}
	render(w, r, "dashboard.html", map[string]interface{}{"Matches": matches, "Active": "match"})
}

// อ่านและตรวจฟอร์มโพสต์ คืนค่า (ข้อมูล, ข้อความ error)
func parseMatchForm(r *http.Request) (Match, string) {
	m := Match{
		TeamName:  strings.TrimSpace(r.PostFormValue("team_name")),
		Title:     strings.TrimSpace(r.PostFormValue("title")),
		PitchType: strings.TrimSpace(r.PostFormValue("pitch_type")),
	}
	n, err := strconv.Atoi(strings.TrimSpace(r.PostFormValue("players_needed")))
	if err == nil {
		m.PlayersNeeded = n
	}
	switch {
	case utf8.RuneCountInString(m.TeamName) < 1 || utf8.RuneCountInString(m.TeamName) > 50:
		return m, "ชื่อทีมต้องยาว 1-50 ตัวอักษร"
	case utf8.RuneCountInString(m.Title) < 1 || utf8.RuneCountInString(m.Title) > 100:
		return m, "หัวข้อต้องยาว 1-100 ตัวอักษร"
	case err != nil || n < 1 || n > 30:
		return m, "จำนวนคนที่ขาดต้องอยู่ระหว่าง 1-30"
	}
	var c int
	if err := db.QueryRow("SELECT COUNT(*) FROM pitches WHERE CONCAT(name, ' (', pitch_type, ')') = ?", m.PitchType).Scan(&c); err != nil {
		log.Println("ERROR:", err)
		return m, "ระบบขัดข้อง กรุณาลองใหม่"
	}
	if c == 0 {
		return m, "กรุณาเลือกสนามจากรายการในระบบ"
	}
	return m, ""
}

func createMatchHandler(w http.ResponseWriter, r *http.Request, v Viewer) {
	pitches, err := loadPitches()
	if err != nil {
		serverError(w, err)
		return
	}
	data := map[string]interface{}{"Pitches": pitches, "Action": "/match/create", "Edit": false, "Match": Match{}, "Active": "match"}
	switch r.Method {
	case http.MethodGet:
		render(w, r, "form.html", data)
	case http.MethodPost:
		if !csrfOK(r) {
			http.Error(w, "403 CSRF token ไม่ถูกต้อง", http.StatusForbidden)
			return
		}
		m, msg := parseMatchForm(r)
		if msg != "" {
			data["Error"], data["Match"] = msg, m
			render(w, r, "form.html", data)
			return
		}
		if _, err := db.Exec("INSERT INTO matches (user_id, team_name, title, pitch_type, players_needed) VALUES (?, ?, ?, ?, ?)",
			v.ID, m.TeamName, m.Title, m.PitchType, m.PlayersNeeded); err != nil {
			serverError(w, err)
			return
		}
		http.Redirect(w, r, "/", http.StatusSeeOther)
	default:
		http.Error(w, "405 Method Not Allowed", http.StatusMethodNotAllowed)
	}
}

func editMatchHandler(w http.ResponseWriter, r *http.Request, v Viewer) {
	if r.Method != http.MethodGet && r.Method != http.MethodPost {
		http.Error(w, "405 Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}
	id, _ := strconv.Atoi(r.FormValue("id"))
	var cur Match
	err := db.QueryRow("SELECT id, user_id, team_name, title, pitch_type, players_needed FROM matches WHERE id = ?", id).
		Scan(&cur.ID, &cur.UserID, &cur.TeamName, &cur.Title, &cur.PitchType, &cur.PlayersNeeded)
	if errors.Is(err, sql.ErrNoRows) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		serverError(w, err)
		return
	}
	if cur.UserID != v.ID { // แก้ได้เฉพาะเจ้าของ
		http.Error(w, "403 Forbidden: คุณไม่ใช่เจ้าของประกาศนี้", http.StatusForbidden)
		return
	}
	pitches, err := loadPitches()
	if err != nil {
		serverError(w, err)
		return
	}
	data := map[string]interface{}{"Pitches": pitches, "Action": "/match/edit", "Edit": true, "Match": cur, "Active": "match"}

	if r.Method == http.MethodGet {
		render(w, r, "form.html", data)
		return
	}
	if !csrfOK(r) {
		http.Error(w, "403 CSRF token ไม่ถูกต้อง", http.StatusForbidden)
		return
	}
	m, msg := parseMatchForm(r)
	m.ID = cur.ID
	if msg == "" {
		var joined int
		if err := db.QueryRow("SELECT COUNT(*) FROM match_joins WHERE match_id = ?", cur.ID).Scan(&joined); err != nil {
			serverError(w, err)
			return
		}
		if m.PlayersNeeded < joined {
			msg = fmt.Sprintf("มีผู้เข้าร่วมแล้ว %d คน จำนวนที่ต้องการต้องไม่น้อยกว่านี้", joined)
		}
	}
	if msg != "" {
		data["Error"], data["Match"] = msg, m
		render(w, r, "form.html", data)
		return
	}
	if _, err := db.Exec("UPDATE matches SET team_name = ?, title = ?, pitch_type = ?, players_needed = ? WHERE id = ? AND user_id = ?",
		m.TeamName, m.Title, m.PitchType, m.PlayersNeeded, cur.ID, v.ID); err != nil {
		serverError(w, err)
		return
	}
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func deleteMatchHandler(w http.ResponseWriter, r *http.Request, v Viewer) {
	id, _ := strconv.Atoi(r.PostFormValue("id"))
	// เงื่อนไข user_id ทำให้ลบได้เฉพาะของตัวเอง
	if _, err := db.Exec("DELETE FROM matches WHERE id = ? AND user_id = ?", id, v.ID); err != nil {
		serverError(w, err)
		return
	}
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func joinMatchHandler(w http.ResponseWriter, r *http.Request, v Viewer) {
	matchID, err := strconv.Atoi(r.PostFormValue("match_id"))
	if err != nil || matchID <= 0 {
		http.Error(w, "400 Bad Request", http.StatusBadRequest)
		return
	}
	tx, err := db.Begin()
	if err != nil {
		serverError(w, err)
		return
	}
	defer tx.Rollback()

	var ownerID, need, cur int
	err = tx.QueryRow("SELECT user_id, players_needed FROM matches WHERE id = ? FOR UPDATE", matchID).Scan(&ownerID, &need)
	if errors.Is(err, sql.ErrNoRows) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		serverError(w, err)
		return
	}
	if ownerID == v.ID {
		http.Error(w, "เจ้าของประกาศไม่ต้องกดเข้าร่วม", http.StatusBadRequest)
		return
	}
	if err := tx.QueryRow("SELECT COUNT(*) FROM match_joins WHERE match_id = ?", matchID).Scan(&cur); err != nil {
		serverError(w, err)
		return
	}
	if cur >= need {
		http.Error(w, "ประกาศนี้เต็มแล้ว", http.StatusConflict)
		return
	}
	if _, err := tx.Exec("INSERT IGNORE INTO match_joins (match_id, user_id) VALUES (?, ?)", matchID, v.ID); err != nil {
		serverError(w, err)
		return
	}
	if err := tx.Commit(); err != nil {
		serverError(w, err)
		return
	}
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

// ==========================================
// Pitches / Bookings
// ==========================================
func pitchesHandler(w http.ResponseWriter, r *http.Request, v Viewer) {
	pitches, err := loadPitches()
	if err != nil {
		serverError(w, err)
		return
	}
	render(w, r, "pitches.html", map[string]interface{}{"Pitches": pitches, "Active": "pitch"})
}

func bookHandler(w http.ResponseWriter, r *http.Request, v Viewer) {
	if r.Method != http.MethodGet && r.Method != http.MethodPost {
		http.Error(w, "405 Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}
	if r.Method == http.MethodPost && !csrfOK(r) {
		http.Error(w, "403 CSRF token ไม่ถูกต้อง", http.StatusForbidden)
		return
	}
	pitchID, _ := strconv.Atoi(r.FormValue("pitch_id"))
	var p Pitch
	err := db.QueryRow("SELECT id, name, price_per_hour FROM pitches WHERE id = ?", pitchID).Scan(&p.ID, &p.Name, &p.PricePerHour)
	if errors.Is(err, sql.ErrNoRows) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		serverError(w, err)
		return
	}

	now := time.Now()
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	data := map[string]interface{}{"Pitch": p, "MinDate": today.Format("2006-01-02"), "Active": "pitch"}
	if r.Method == http.MethodGet {
		render(w, r, "book.html", data)
		return
	}

	d, derr := time.Parse("2006-01-02", r.PostFormValue("booking_date"))
	t, terr := time.Parse("15:04", r.PostFormValue("start_time"))
	phone := cleanPhone(r.PostFormValue("contact_phone"))
	msg := ""
	switch {
	case derr != nil || terr != nil:
		msg = "รูปแบบวันที่หรือเวลาไม่ถูกต้อง"
	case d.Before(today):
		msg = "ไม่สามารถจองย้อนหลังได้"
	case d.After(today.AddDate(0, 6, 0)):
		msg = "จองล่วงหน้าได้ไม่เกิน 6 เดือน"
	case d.Equal(today) && t.Hour() <= now.Hour():
		msg = "เวลานี้ผ่านไปแล้ว"
	case !phoneRe.MatchString(phone):
		msg = "เบอร์โทรศัพท์ไม่ถูกต้อง (เช่น 0812345678)"
	}
	if msg == "" {
		start := fmt.Sprintf("%02d:00:00", t.Hour()) // จองเป็นรายชั่วโมง
		_, err := db.Exec("INSERT INTO bookings (user_id, pitch_id, booking_date, start_time, contact_phone) VALUES (?, ?, ?, ?, ?)",
			v.ID, p.ID, d.Format("2006-01-02"), start, phone)
		if err == nil {
			http.Redirect(w, r, "/my-bookings", http.StatusSeeOther)
			return
		}
		if !isDup(err) { // UNIQUE(pitch_id, booking_date, start_time) กันจองซ้ำ
			serverError(w, err)
			return
		}
		msg = "ช่วงเวลานี้ถูกจองแล้ว กรุณาเลือกเวลาอื่น"
	}
	data["Error"] = msg
	render(w, r, "book.html", data)
}

func myBookingsHandler(w http.ResponseWriter, r *http.Request, v Viewer) {
	bookings, err := loadMyBookings(v.ID)
	if err != nil {
		serverError(w, err)
		return
	}
	render(w, r, "my_bookings.html", map[string]interface{}{"Bookings": bookings, "Active": "mine"})
}

func cancelBookingHandler(w http.ResponseWriter, r *http.Request, v Viewer) {
	id, _ := strconv.Atoi(r.PostFormValue("id"))
	if _, err := db.Exec("DELETE FROM bookings WHERE id = ? AND user_id = ?", id, v.ID); err != nil {
		serverError(w, err)
		return
	}
	http.Redirect(w, r, "/my-bookings", http.StatusSeeOther)
}

// ==========================================
// Admin
// ==========================================
func adminHandler(w http.ResponseWriter, r *http.Request, v Viewer) {
	matches, err := loadAdminMatches()
	if err != nil {
		serverError(w, err)
		return
	}
	bookings, err := loadAdminBookings()
	if err != nil {
		serverError(w, err)
		return
	}
	reviews, err := loadAdminReviews()
	if err != nil {
		serverError(w, err)
		return
	}
	render(w, r, "admin.html", map[string]interface{}{"Matches": matches, "Bookings": bookings, "Reviews": reviews})
}

func adminDeleteMatchHandler(w http.ResponseWriter, r *http.Request, v Viewer) {
	id, _ := strconv.Atoi(r.PostFormValue("id"))
	if _, err := db.Exec("DELETE FROM matches WHERE id = ?", id); err != nil {
		serverError(w, err)
		return
	}
	http.Redirect(w, r, "/admin", http.StatusSeeOther)
}

func adminApproveBookingHandler(w http.ResponseWriter, r *http.Request, v Viewer) {
	id, _ := strconv.Atoi(r.PostFormValue("id"))
	if _, err := db.Exec("UPDATE bookings SET status = 'Approved' WHERE id = ?", id); err != nil {
		serverError(w, err)
		return
	}
	http.Redirect(w, r, "/admin", http.StatusSeeOther)
}

func adminDeleteBookingHandler(w http.ResponseWriter, r *http.Request, v Viewer) {
	id, _ := strconv.Atoi(r.PostFormValue("id"))
	if _, err := db.Exec("DELETE FROM bookings WHERE id = ?", id); err != nil {
		serverError(w, err)
		return
	}
	http.Redirect(w, r, "/admin", http.StatusSeeOther)
}

// ==========================================
// Reviews (รีวิว/ให้คะแนนสนาม) – 1 คนรีวิวได้ 1 ครั้งต่อสนาม แก้ไข/ลบได้เฉพาะของตัวเอง
// ==========================================
func reviewsHandler(w http.ResponseWriter, r *http.Request, v Viewer) {
	if r.Method != http.MethodGet && r.Method != http.MethodPost {
		http.Error(w, "405 Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}
	pitchID, _ := strconv.Atoi(r.FormValue("pitch_id"))
	pitches, err := loadPitches()
	if err != nil {
		serverError(w, err)
		return
	}
	var pitch *Pitch
	for i := range pitches {
		if pitches[i].ID == pitchID {
			pitch = &pitches[i]
		}
	}
	if pitch == nil {
		http.NotFound(w, r)
		return
	}

	msg := ""
	postedRating, postedComment := 0, ""
	if r.Method == http.MethodPost {
		if !csrfOK(r) {
			http.Error(w, "403 CSRF token ไม่ถูกต้อง", http.StatusForbidden)
			return
		}
		rating, rerr := strconv.Atoi(r.PostFormValue("rating"))
		comment := strings.TrimSpace(r.PostFormValue("comment"))
		postedRating, postedComment = rating, comment
		switch {
		case rerr != nil || rating < 1 || rating > 5:
			msg = "กรุณาให้คะแนน 1-5 ดาว"
		case utf8.RuneCountInString(comment) > 500:
			msg = "ความคิดเห็นยาวเกิน 500 ตัวอักษร"
		}
		if msg == "" {
			// user_id มาจาก session เท่านั้น จึงแก้ได้เฉพาะรีวิวของตัวเอง
			if _, err := db.Exec(`INSERT INTO reviews (pitch_id, user_id, rating, comment) VALUES (?, ?, ?, ?)
				ON DUPLICATE KEY UPDATE rating = VALUES(rating), comment = VALUES(comment)`,
				pitchID, v.ID, rating, comment); err != nil {
				serverError(w, err)
				return
			}
			http.Redirect(w, r, "/reviews?pitch_id="+strconv.Itoa(pitchID), http.StatusSeeOther)
			return
		}
	}

	reviews, err := loadReviews(pitchID)
	if err != nil {
		serverError(w, err)
		return
	}
	hasMine := false
	myRating, myComment := 0, ""
	for i := range reviews {
		reviews[i].IsOwner = reviews[i].UserID == v.ID
		if reviews[i].IsOwner {
			hasMine, myRating, myComment = true, reviews[i].Rating, reviews[i].Comment
		}
	}
	if msg != "" {
		myRating, myComment = postedRating, postedComment
	}
	render(w, r, "reviews.html", map[string]interface{}{
		"Pitch": pitch, "Reviews": reviews, "HasMine": hasMine,
		"MyRating": myRating, "MyComment": myComment, "Error": msg, "Active": "pitch",
	})
}

func deleteReviewHandler(w http.ResponseWriter, r *http.Request, v Viewer) {
	id, _ := strconv.Atoi(r.PostFormValue("id"))
	pitchID, _ := strconv.Atoi(r.PostFormValue("pitch_id"))
	if _, err := db.Exec("DELETE FROM reviews WHERE id = ? AND user_id = ?", id, v.ID); err != nil {
		serverError(w, err)
		return
	}
	http.Redirect(w, r, "/reviews?pitch_id="+strconv.Itoa(pitchID), http.StatusSeeOther)
}

func adminDeleteReviewHandler(w http.ResponseWriter, r *http.Request, v Viewer) {
	id, _ := strconv.Atoi(r.PostFormValue("id"))
	if _, err := db.Exec("DELETE FROM reviews WHERE id = ?", id); err != nil {
		serverError(w, err)
		return
	}
	http.Redirect(w, r, "/admin", http.StatusSeeOther)
}

// ==========================================
// API (ต้องล็อกอิน และไม่ส่งเบอร์โทร)
// ==========================================
type apiMatch struct {
	ID            int    `json:"id"`
	Title         string `json:"title"`
	PitchType     string `json:"pitch_type"`
	PlayersNeeded int    `json:"players_needed"`
	TeamName      string `json:"team_name"`
	Owner         string `json:"owner"`
}

func apiGetMatchesHandler(w http.ResponseWriter, r *http.Request, v Viewer) {
	rows, err := db.Query(`SELECT m.id, m.title, m.pitch_type, m.players_needed, m.team_name, u.username
		FROM matches m JOIN users u ON m.user_id = u.id ORDER BY m.id DESC`)
	if err != nil {
		http.Error(w, `{"error":"database error"}`, http.StatusInternalServerError)
		return
	}
	defer rows.Close()
	out := []apiMatch{}
	for rows.Next() {
		var m apiMatch
		if err := rows.Scan(&m.ID, &m.Title, &m.PitchType, &m.PlayersNeeded, &m.TeamName, &m.Owner); err != nil {
			http.Error(w, `{"error":"database error"}`, http.StatusInternalServerError)
			return
		}
		out = append(out, m)
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(out)
}
