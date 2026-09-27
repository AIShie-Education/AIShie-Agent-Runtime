## Why you lost marks on HW3

Your `parse_scores` function reads the file correctly, but question 2 asked
for the **median**, and your code returns the mean. See the
[statistics module](https://docs.python.org/3/library/statistics.html) and
`median`.

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
summary
and you can view it here
or at [link removed] or
here.

Open the report or email
[link removed].

[docs]: https://docs.python.org/3/

    # An indented block after a blank line, outside any list, is code:
    curl "https://api.example.edu/v1/grades?student=me"

<!-- {LinksRemoved:7 ImagesRemoved:1 Truncated:false Empty:false} -->
