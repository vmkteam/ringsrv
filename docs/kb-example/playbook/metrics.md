---
description: Определения метрик магазина — заказы, выручка, конверсия. Читать до любого расчёта.
driver: postgres
---
# Метрики

## Заказы

Заказ — строка `orders`. Отменённые (`status = 'cancelled'`) в «заказы» не входят, кроме графиков входящего потока.

## Выручка

Выручка магазина — **комиссия**, 10% от суммы заказа, а не сама сумма. Сумма заказов — оборот (GMV).

```sql
SELECT date_trunc('week', created_at) AS week,
       sum(amount)        AS gmv,
       sum(amount) * 0.10 AS revenue
FROM orders
WHERE status <> 'cancelled'
GROUP BY 1
ORDER BY 1
```

Фрагмент, который подставляют в любой запрос по оплаченным заказам. Он не запрос сам по себе, поэтому помечен `sql-expr`, а не `sql`: проверка пакета прогоняет через шлюз только `sql`-блоки:

```sql-expr
WHERE status = 'paid' AND paid_at IS NOT NULL
```

Запись сервер не пропустит — это пример того, чего делать нельзя:

```sql-rejected
UPDATE orders SET status = 'paid'
```

## Конверсия

Конверсия — заказы, делённые на сессии с просмотром карточки товара, см. kb://shop/playbook/funnel.md.
