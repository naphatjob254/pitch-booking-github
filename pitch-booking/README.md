# J MATCH ARENA — ระบบจองสนามฟุตบอล & หาคนเตะ

เว็บแอป Go (net/http + MySQL) : สมัคร/ล็อกอิน, ประกาศหาคนเตะ, จองสนาม, รีวิวสนาม, หน้าแอดมิน

## โครงสร้าง
| ไฟล์ | หน้าที่ |
|---|---|
| `main.go` | route, middleware, session/CSRF, handler |
| `db.go` | เชื่อมต่อ MySQL, โมเดล, query ทั้งหมดที่ใช้อ่านข้อมูล |
| `ui.go` | ฝัง `templates/` และ `static/` เข้าไบนารีด้วย `go:embed` |
| `templates/` | หน้า HTML (html/template) |
| `schema.sql` | โครงสร้างฐานข้อมูล + ข้อมูลสนามเริ่มต้น |

## เริ่มใช้งาน
```bash
# 1) สร้างฐานข้อมูล
mysql -u root -p < schema.sql

# 2) ตั้งค่า (ดู .env.example)
export DB_DSN='root:รหัส@tcp(127.0.0.1:3306)/pitch_booking?charset=utf8mb4'
export SESSION_KEY="$(openssl rand -hex 32)"

# 3) รัน
go run .                      # http://localhost:8080

# สร้างแอดมิน
go run . create-admin <username> <phone> <password>
```

## เปิดให้คนนอกเข้า (Cloudflare Tunnel)
```bash
cloudflared tunnel --url http://localhost:8080
```
ตั้ง `COOKIE_SECURE=1` และ `TRUST_PROXY=1` ตอนใช้วิธีนี้
