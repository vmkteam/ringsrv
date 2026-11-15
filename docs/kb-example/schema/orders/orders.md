---
table: orders
source: orders
purpose: Заказы магазина, одна строка на заказ; главная таблица для выручки и конверсии.
grain: заказ
columns:
  - name: id
    description: Идентификатор заказа.
  - name: status
    description: Исход заказа. Отменённые не входят в «заказы» (playbook/metrics).
    enum: [new, paid, cancelled]
  - name: amount
    description: Сумма заказа в рублях — оборот, не выручка.
  - name: paid_at
    description: Время оплаты; пусто, пока заказ не оплачен.
notes:
  - Выручка — 10% от amount, см. kb://shop/playbook/metrics.md.
relations:
  - orders.user_id → users.id
---
Заказы пишет сервис заказов; статус меняется, строка не пересоздаётся.
