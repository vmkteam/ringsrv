---
description: Заказы и выручка по неделям, как на дашборде «Продажи».
driver: postgres
---
```sql
SELECT date_trunc('week', created_at) AS week,
       count(*)                       AS orders,
       sum(amount) * 0.10             AS revenue
FROM orders
WHERE status <> 'cancelled'
GROUP BY 1
ORDER BY 1 DESC
LIMIT 12
```
