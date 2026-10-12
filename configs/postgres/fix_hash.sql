UPDATE users SET
  password_hash = 'pbkdf2$sha256$120000$JsA5/oJhj1jpabjJRlbQFA$4/FhHWEpdOZoO0qLkz/2qCKLJCwZ4uoKt7JYrxkdGWo'
WHERE username IN ('admin', 'alice', 'bob', 'disabled');
