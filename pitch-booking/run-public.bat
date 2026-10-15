@echo off
rem ตั้งค่า SESSION_KEY ใน environment ของเครื่องก่อน (ห้ามเขียนคีย์จริงในไฟล์นี้ เพราะจะขึ้น GitHub)
rem   setx SESSION_KEY "ค่าสุ่มยาว 32+ ตัวอักษร"
cd /d "%~dp0"
if not defined SESSION_KEY (
  echo [ERROR] ยังไม่ได้ตั้ง SESSION_KEY
  pause
  exit /b 1
)
set COOKIE_SECURE=1
set TRUST_PROXY=1
go run .
pause
