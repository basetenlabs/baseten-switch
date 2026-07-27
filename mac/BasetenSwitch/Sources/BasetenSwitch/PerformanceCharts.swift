import Charts
import SwiftUI

struct PerformanceCharts: View {
    let rows: [TrafficPerformanceRow]
    @Environment(\.accessibilityReduceMotion) private var reduceMotion

    var body: some View {
        HStack(alignment: .top, spacing: 18) {
            metricChart(
                title: "Median time to first token",
                unit: "ms",
                value: \.measuredMedianTTFTMS)
            metricChart(
                title: "Median output speed",
                unit: "tok/s",
                value: \.measuredMedianOutputTokensPerSecond)
        }
    }

    private func metricChart(
        title: String,
        unit: String,
        value: KeyPath<TrafficPerformanceRow, Double?>
    ) -> some View {
        VStack(alignment: .leading, spacing: 10) {
            Text(title)
                .font(.headline)
            Chart(rows) { row in
                if let metric = row[keyPath: value] {
                    BarMark(
                        x: .value(title, metric),
                        y: .value("Provider or model", row.label)
                    )
                    .foregroundStyle(
                        row.provider.caseInsensitiveCompare("Baseten")
                            == .orderedSame
                            ? Color.basetenGreen
                            : Color.claudeTerracotta)
                    .annotation(position: .trailing, alignment: .leading) {
                        Text(
                            "\(metric.formatted(.number.precision(.fractionLength(0...1)))) \(unit)")
                            .font(.caption.monospacedDigit())
                            .foregroundStyle(.secondary)
                    }
                }
            }
            .chartXAxis {
                AxisMarks(position: .bottom)
            }
            .chartXScale(
                domain: 0...chartMaximum(value: value))
            .animation(
                TrafficChartMotion.animation(reduceMotion: reduceMotion),
                value: rows)
            .frame(height: max(130, CGFloat(rows.count) * 36))
            .accessibilityLabel(title)
            .accessibilityValue(
                rows.compactMap { row in
                    row[keyPath: value].map {
                        "\(row.label), \($0.formatted()) \(unit)"
                    }
                }.joined(separator: "; "))
        }
        .frame(maxWidth: .infinity, alignment: .leading)
    }

    private func chartMaximum(
        value: KeyPath<TrafficPerformanceRow, Double?>
    ) -> Double {
        max(
            1,
            (rows.compactMap { $0[keyPath: value] }.max() ?? 0) * 1.34)
    }
}
