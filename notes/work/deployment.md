# Deployment notes — billing service v2

Target window: Thursday after 6 PM, when traffic is lowest.

Checklist:
- Run the database migration `0042_invoice_currency` first; it takes about ten minutes on production.
- Deploy to the canary pool (5% of traffic) and watch error rates and p95 latency for 30 minutes.
- If the canary is clean, roll out to the rest in two waves.
- Rollback: redeploy the previous image tag and run `0042_down`. The down migration is safe because the new column is nullable.

Open questions:
- Priya (priya.k@example.com) still needs to confirm the Stripe webhook secret was rotated.
- Ask the on-call to silence the "invoice lag" alert during the migration.
