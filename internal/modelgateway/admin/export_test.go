package admin

// SetMaxDailyRows sets the rollups one daily read answers for one test, and
// returns its restore.
func SetMaxDailyRows(n int) func() {
	old := maxDailyRows
	maxDailyRows = n
	return func() { maxDailyRows = old }
}
