import Foundation

/// hhmmLocal renders a bare "HH:mm" wall-clock in the user's local zone —
/// used by the menu's expiry columns where a date would waste horizontal
/// space and the year/day never actually differs from today.
func hhmmLocal(_ d: Date) -> String {
    let f = DateFormatter()
    f.dateFormat = "HH:mm"
    f.timeZone = .current
    return f.string(from: d)
}

/// localTimestamp renders a full "yyyy-MM-dd HH:mm:ss zzz" in the user's
/// local zone — for the diagnostics window and clipboard-copy, where an
/// unambiguous absolute reference matters more than glance-brevity.
func localTimestamp(_ d: Date) -> String {
    let f = DateFormatter()
    f.dateFormat = "yyyy-MM-dd HH:mm:ss zzz"
    f.timeZone = .current
    return f.string(from: d)
}

/// untilNow renders "in 5h 42m" / "expired 3m ago" relative to now. Both
/// menu and diagnostics use it so the humanization is consistent.
func untilNow(_ d: Date) -> String {
    let delta = d.timeIntervalSince(Date())
    if delta < 0 { return "expired \(shortDuration(-delta)) ago" }
    return "in \(shortDuration(delta))"
}

/// agoNow renders "42s ago" / "3m ago" — companion of untilNow for
/// past-facing "last sync at X".
func agoNow(_ d: Date) -> String {
    let delta = Date().timeIntervalSince(d)
    if delta < 60 { return "\(Int(delta))s ago" }
    return "\(Int(delta / 60))m ago"
}

/// shortDuration formats a seconds count as "42s" / "5m" / "2h 15m". Kept
/// out of untilNow/agoNow so both can share the exact rounding rules.
func shortDuration(_ seconds: TimeInterval) -> String {
    let s = Int(seconds)
    if s < 60 { return "\(s)s" }
    if s < 3600 { return "\(s / 60)m" }
    let hours = s / 3600
    let mins = (s % 3600) / 60
    return mins == 0 ? "\(hours)h" : "\(hours)h \(mins)m"
}

/// logFileURL points at the sync log the launchd unit historically wrote
/// to. We keep the same file even under the menubar app so operators
/// grepping old paths still find fresh entries.
func logFileURL() -> URL {
    URL(fileURLWithPath: NSHomeDirectory())
        .appendingPathComponent("Library/Logs/multica-token-sync.log")
}
