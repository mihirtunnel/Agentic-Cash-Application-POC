## 2nd run of single agent. 
```
========================================
 PIPELINE RESULTS SUMMARY
========================================

Transactions processed:    32
  Matched by agent:       27  (84%)
  Internal transfers:     4  (12%)
  Unresolved:             1  (3%)

Routing decisions:
  Internal transfer:      0  (0%)
  Fast-track review:      25  (78%)
  Human review:           2  (6%)
  Manual investigation:   5  (16%)

Accuracy (vs expected):
  Correct customer ID:      23/32
  Correct invoice match:    19/32
  Correct internal transfer: 4/32

AI costs:
  Total Claude API cost:  $0.3919
  Avg cost per txn:       $0.0122
  Total tool calls:       196
  Avg tool calls per txn: 6.1

  Projected daily cost (500 txns): $6.12
  Projected monthly cost:          $183.68

```


## Multi-Trx results:
```
========================================
 PIPELINE RESULTS SUMMARY
========================================

Transactions processed:    32
  Matched by agent:       24  (75%)
  Internal transfers:     6  (19%)
  Unresolved:             2  (6%)

Routing decisions:
  Internal transfer:      0  (0%)
  Fast-track review:      23  (72%)
  Human review:           1  (3%)
  Manual investigation:   8  (25%)

Accuracy (vs expected):
  Correct customer ID:      20/32
  Correct invoice match:    20/32
  Correct internal transfer: 5/32

AI costs:
  Total Claude API cost:  $0.7482
  Avg cost per txn:       $0.0234
  Total tool calls:       0
  Avg tool calls per txn: 0.0

  Projected daily cost (500 txns): $11.69
  Projected monthly cost:          $350.72
  
```