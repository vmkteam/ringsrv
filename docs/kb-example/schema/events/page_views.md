---
table: page_views
source: events
purpose: Просмотры страниц сайта, одна строка на просмотр.
grain: просмотр
columns:
  - name: session_id
    description: Сессия посетителя.
  - name: kind
    description: Тип страницы, шаг воронки.
    enum: [catalog, product, cart, checkout]
  - name: ts
    description: Время просмотра, UTC.
notes:
  - ReplacingMergeTree — FINAL обязателен, иначе дубли.
---
