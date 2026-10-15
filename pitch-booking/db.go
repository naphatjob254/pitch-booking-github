package main

// db.go — ชั้นข้อมูลทั้งหมด: การเชื่อมต่อ MySQL, โมเดล และ query ที่ handler เรียกใช้
// (handler ใน main.go ไม่เขียน SQL ฟังก์ชันอ่านข้อมูลเอง ยกเว้นการเขียน/ลบข้อมูลเล็ก ๆ)

import (
	"database/sql"
	"errors"
	"log"
	"math"
	"strings"
	"time"

	"github.com/go-sql-driver/mysql"
)

var db *sql.DB

// ==========================================
// Models
// ==========================================
type MatchJoiner struct {
	Username string
	Phone    string // จะมีค่าเฉพาะตอนผู้ดูเป็นเจ้าของโพสต์
}

type Match struct {
	ID            int
	UserID        int
	OwnerName     string
	OwnerPhone    string
	TeamName      string
	Title         string
	PitchType     string
	PlayersNeeded int
	CurrentJoin   int
	HasJoined     bool
	IsOwner       bool
	Joiners       []MatchJoiner
}

type Pitch struct {
	ID           int
	Name         string
	PitchType    string
	PricePerHour int
	AvgRating    float64
	ReviewCount  int
	Stars        string
}

type Review struct {
	ID        int
	UserID    int
	Username  string
	Rating    int
	Comment   string
	Stars     string
	UpdatedAt string
	IsOwner   bool
}

type AdminReview struct {
	ID        int
	Username  string
	PitchName string
	Rating    int
	Stars     string
	Comment   string
}

func stars(n int) string {
	if n < 0 {
		n = 0
	}
	if n > 5 {
		n = 5
	}
	return strings.Repeat("★", n) + strings.Repeat("☆", 5-n)
}

type Booking struct {
	ID          int
	PitchName   string
	BookingDate string
	StartTime   string
	Status      string
}

type AdminMatch struct {
	ID            int
	Username      string
	TeamName      string
	Title         string
	PitchType     string
	PlayersNeeded int
}

type AdminBooking struct {
	ID          int
	Username    string
	Phone       string
	PitchName   string
	BookingDate string
	StartTime   string
	Status      string
}

// ==========================================
// Connection
// ==========================================
func initDB() {
	var err error
	dsn := env("DB_DSN", "root:@tcp(127.0.0.1:3306)/pitch_booking?charset=utf8mb4")
	db, err = sql.Open("mysql", dsn)
	if err != nil {
		log.Fatal("DB Connect Error: ", err)
	}
	db.SetMaxOpenConns(20)
	db.SetConnMaxLifetime(5 * time.Minute)
	if err = db.Ping(); err != nil {
		log.Fatal("DB Ping Error: ", err)
	}
}

// isDup: true เมื่อ error เป็น duplicate key (MySQL 1062)
func isDup(err error) bool {
	var me *mysql.MySQLError
	return errors.As(err, &me) && me.Number == 1062
}

// hhmm: "19:00:00" -> "19:00"
func hhmm(s string) string {
	if len(s) >= 5 {
		return s[:5]
	}
	return s
}

// ==========================================
// Pitches
// ==========================================
func loadPitches() ([]Pitch, error) {
	rows, err := db.Query(`SELECT p.id, p.name, p.pitch_type, p.price_per_hour,
			COALESCE(ROUND(AVG(r.rating), 1), 0), COUNT(r.id)
		FROM pitches p LEFT JOIN reviews r ON r.pitch_id = p.id
		GROUP BY p.id, p.name, p.pitch_type, p.price_per_hour ORDER BY p.id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	pitches := []Pitch{}
	for rows.Next() {
		var p Pitch
		if err := rows.Scan(&p.ID, &p.Name, &p.PitchType, &p.PricePerHour, &p.AvgRating, &p.ReviewCount); err != nil {
			return nil, err
		}
		p.Stars = stars(int(math.Round(p.AvgRating)))
		pitches = append(pitches, p)
	}
	return pitches, rows.Err()
}

// ==========================================
// Matches (หาคนเตะ)
// ==========================================
// loadMatches: ประกาศทั้งหมด + ผู้เข้าร่วม; เบอร์ผู้เข้าร่วมเห็นได้เฉพาะเจ้าของโพสต์
func loadMatches(viewerID int) ([]Match, error) {
	rows, err := db.Query(`SELECT m.id, m.title, m.pitch_type, m.players_needed, m.user_id, u.username, u.phone, m.team_name
		FROM matches m JOIN users u ON m.user_id = u.id ORDER BY m.id DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	matches := []Match{}
	idx := map[int]int{}
	for rows.Next() {
		var m Match
		if err := rows.Scan(&m.ID, &m.Title, &m.PitchType, &m.PlayersNeeded, &m.UserID, &m.OwnerName, &m.OwnerPhone, &m.TeamName); err != nil {
			return nil, err
		}
		m.IsOwner = m.UserID == viewerID
		idx[m.ID] = len(matches)
		matches = append(matches, m)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	jrows, err := db.Query(`SELECT mj.match_id, mj.user_id, u.username, u.phone
		FROM match_joins mj JOIN users u ON mj.user_id = u.id ORDER BY mj.match_id, u.username`)
	if err != nil {
		return nil, err
	}
	defer jrows.Close()
	for jrows.Next() {
		var mid, uid int
		var uname, phone string
		if err := jrows.Scan(&mid, &uid, &uname, &phone); err != nil {
			return nil, err
		}
		i, ok := idx[mid]
		if !ok {
			continue
		}
		m := &matches[i]
		j := MatchJoiner{Username: uname}
		if m.IsOwner {
			j.Phone = phone
		}
		m.Joiners = append(m.Joiners, j)
		m.CurrentJoin++
		if uid == viewerID {
			m.HasJoined = true
		}
	}
	if err := jrows.Err(); err != nil {
		return nil, err
	}
	return matches, nil
}

// ==========================================
// Bookings
// ==========================================
func loadMyBookings(userID int) ([]Booking, error) {
	rows, err := db.Query(`SELECT b.id, p.name, b.booking_date, b.start_time, b.status
		FROM bookings b JOIN pitches p ON b.pitch_id = p.id
		WHERE b.user_id = ? ORDER BY b.booking_date DESC, b.start_time DESC`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	bookings := []Booking{}
	for rows.Next() {
		var b Booking
		if err := rows.Scan(&b.ID, &b.PitchName, &b.BookingDate, &b.StartTime, &b.Status); err != nil {
			return nil, err
		}
		b.StartTime = hhmm(b.StartTime)
		bookings = append(bookings, b)
	}
	return bookings, rows.Err()
}

// ==========================================
// Reviews
// ==========================================
// loadReviews: รีวิวของสนาม เรียงใหม่สุดก่อน (IsOwner ให้ handler เติมเอง)
func loadReviews(pitchID int) ([]Review, error) {
	rows, err := db.Query(`SELECT r.id, r.user_id, u.username, r.rating, r.comment, DATE_FORMAT(r.updated_at, '%Y-%m-%d')
		FROM reviews r JOIN users u ON r.user_id = u.id WHERE r.pitch_id = ? ORDER BY r.updated_at DESC`, pitchID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	reviews := []Review{}
	for rows.Next() {
		var rv Review
		if err := rows.Scan(&rv.ID, &rv.UserID, &rv.Username, &rv.Rating, &rv.Comment, &rv.UpdatedAt); err != nil {
			return nil, err
		}
		rv.Stars = stars(rv.Rating)
		reviews = append(reviews, rv)
	}
	return reviews, rows.Err()
}

// ==========================================
// Admin
// ==========================================
func loadAdminMatches() ([]AdminMatch, error) {
	rows, err := db.Query(`SELECT m.id, u.username, m.team_name, m.title, m.pitch_type, m.players_needed
		FROM matches m JOIN users u ON m.user_id = u.id ORDER BY m.id DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []AdminMatch{}
	for rows.Next() {
		var m AdminMatch
		if err := rows.Scan(&m.ID, &m.Username, &m.TeamName, &m.Title, &m.PitchType, &m.PlayersNeeded); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

func loadAdminBookings() ([]AdminBooking, error) {
	rows, err := db.Query(`SELECT b.id, u.username, COALESCE(b.contact_phone, ''), p.name, b.booking_date, b.start_time, b.status
		FROM bookings b JOIN users u ON b.user_id = u.id JOIN pitches p ON b.pitch_id = p.id
		ORDER BY b.booking_date DESC, b.start_time DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []AdminBooking{}
	for rows.Next() {
		var b AdminBooking
		if err := rows.Scan(&b.ID, &b.Username, &b.Phone, &b.PitchName, &b.BookingDate, &b.StartTime, &b.Status); err != nil {
			return nil, err
		}
		b.StartTime = hhmm(b.StartTime)
		out = append(out, b)
	}
	return out, rows.Err()
}

func loadAdminReviews() ([]AdminReview, error) {
	rows, err := db.Query(`SELECT r.id, u.username, p.name, r.rating, r.comment
		FROM reviews r JOIN users u ON r.user_id = u.id JOIN pitches p ON r.pitch_id = p.id ORDER BY r.id DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []AdminReview{}
	for rows.Next() {
		var rv AdminReview
		if err := rows.Scan(&rv.ID, &rv.Username, &rv.PitchName, &rv.Rating, &rv.Comment); err != nil {
			return nil, err
		}
		rv.Stars = stars(rv.Rating)
		out = append(out, rv)
	}
	return out, rows.Err()
}
