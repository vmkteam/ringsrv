---
name: weekly-report
description: Недельный отчёт по заказам и выручке с проверкой определений.
arguments:
  - name: weeks
    description: Сколько недель показать (по умолчанию 4).
    required: false
---
Собери отчёт по заказам и выручке за последние {{weeks}} недель.

Сначала прочитай help(["playbook/metrics"]): выручка — комиссия, не оборот. Возьми запрос из help(["queries/orders-by-week"]), назови определения в ответе.
