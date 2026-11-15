---
description: Воронка сайта по событиям — от просмотра карточки до заказа.
driver: clickhouse
---
# Воронка

Шаги воронки — события `page_views` с `kind` из kb://shop/schema/events/page_views.md.
Свежие сутки неполные: бери `now() - INTERVAL 2 HOUR` как правую границу.

```sql
SELECT kind, uniqExact(session_id) AS sessions
FROM page_views FINAL
WHERE ts >= today() - 7 AND ts < now() - INTERVAL 2 HOUR
GROUP BY kind
ORDER BY sessions DESC
```
