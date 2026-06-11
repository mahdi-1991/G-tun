# 🚀 راهنمای آپدیت سرور

## ✅ آماده برای Commit

همه فایل‌ها آماده شدن. برای commit و push:

```bash
cd "c:\Users\Mahdi\Documents\kiro\G-tun"

# کامیت کردن
git commit -m "🐛 Critical Bug Fixes & Performance Improvements

- Add missing quic-go dependency (v0.48.2)
- Fix UDP session memory leak with auto-cleanup
- Fix client reconnect flood with exponential backoff
- Fix smux session resource leak
- Add auth timeout protection

See CHANGELOG.md for details"

# پوش کردن به GitHub
git push origin main
```

---

## 📋 فایل‌های تغییر یافته

### کد اصلی (Bug Fixes):
- ✅ `server/server.go` - رفع UDP leak + auth timeout + بهبود error handling
- ✅ `client/client.go` - رفع UDP leak + exponential backoff + بهبود session management
- ✅ `server/go.mod` - اضافه شدن quic-go@v0.48.2
- ✅ `client/go.mod` - اضافه شدن quic-go@v0.48.2

### اسکریپت‌ها (Deployment):
- ✅ `install.sh` - آپدیت به quic-go@v0.48.2
- ✅ `g-tun.sh` - بهبود گزینه 8 (update function)

### مستندسازی:
- ✅ `README.md` - اضافه شدن بخش Update & Bug Fixes
- ✅ `CHANGELOG.md` - مستندسازی کامل تغییرات
- ✅ `.gitignore` - جلوگیری از commit فایل‌های اضافی

---

## 🎯 مراحل آپدیت روی سرور

بعد از push کردن، روی هر سرور Ubuntu:

```bash
# 1. باز کردن پنل مدیریت
g-tun

# 2. انتخاب گزینه 8
# "8) Update G-Tun to Latest Version"

# 3. صبر تا پروسه تموم بشه
# - Git pull
# - Go mod tidy
# - Build
# - Restart service

# 4. چک کردن وضعیت
g-tun
# Status باید [RUNNING] نشون بده
```

---

## 🔍 تست کردن بعد از آپدیت

### روی Server:
```bash
# چک کردن log ها
journalctl -u g-tun-server -f

# باید ببینی:
# "Server starting — control port: 8080, data port: 8081, protocol: quic"
# "Control listener ready on port 8080"
```

### روی Client:
```bash
# چک کردن log ها
journalctl -u g-tun-client -f

# باید ببینی:
# "Connecting to control server..."
# "Ready! Listening locally on 0.0.0.0:1080"
```

---

## 🐛 مشکلات رفع شده

این آپدیت باگ‌های زیر رو برطرف می‌کنه:

1. **Build Error با QUIC** ❌ → ✅
   - قبلاً: `cannot find package "github.com/quic-go/quic-go"`
   - حالا: Dependency اضافه شده، build موفق

2. **Memory Leak در UDP** ❌ → ✅
   - قبلاً: Sessionها تا ابد می‌موندن
   - حالا: بعد از 3 دقیقه idle، پاک می‌شن

3. **Client Reconnect Flood** ❌ → ✅
   - قبلاً: هر 3 ثانیه retry (server رو flood می‌کرد)
   - حالا: 3s → 6s → 12s → ... → 60s (exponential backoff)

4. **Smux Session Leak** ❌ → ✅
   - قبلاً: Session قدیمی بسته نمی‌شد
   - حالا: قبل از ساخت session جدید، قدیمی بسته می‌شه

5. **Control Connection Hang** ❌ → ✅
   - قبلاً: اگه کلاینت hang می‌کرد، goroutine تا ابد می‌موند
   - حالا: 10 ثانیه timeout برای auth

---

## ⚠️ نکات مهم

- ✅ **هیچ config ای از بین نمی‌ره** - token و certificate ها سالم می‌مونن
- ✅ **Downtime نداره** - service به صورت خودکار restart می‌شه (< 1 ثانیه)
- ✅ **سازگار با کلاینت‌های قدیمی** - نیازی به آپدیت همزمان نیست
- ⚠️ **فقط از طریق `g-tun` آپدیت کن** - دستی build نکن

---

## 📞 اگر مشکلی پیش اومد

```bash
# نگاه به log ها
journalctl -u g-tun-server -n 50

# یا
journalctl -u g-tun-client -n 50

# restart دستی
systemctl restart g-tun-server
# یا
systemctl restart g-tun-client
```

---

موفق باشی! 🎉
