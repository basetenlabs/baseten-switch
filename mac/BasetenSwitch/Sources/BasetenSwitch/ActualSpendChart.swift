import Charts
import SwiftUI

extension Color {
    static let basetenGreen = Color(
        red: 22 / 255,
        green: 215 / 255,
        blue: 102 / 255)
    static let claudeTerracotta = Color(
        red: 217 / 255,
        green: 119 / 255,
        blue: 87 / 255)
}

struct ActualSpendChart: View {
    let rows: [TrafficCostRow]
    @Environment(\.accessibilityReduceMotion) private var reduceMotion

    private var pricedRows: [TrafficCostRow] {
        rows.filter { $0.actualCostUSD != nil }
    }

    @ViewBuilder
    var body: some View {
        if pricedRows.isEmpty {
            Text("No priced spend is available for this range.")
                .font(.callout)
                .foregroundStyle(.secondary)
                .frame(maxWidth: .infinity, minHeight: 80)
        } else {
            Chart(pricedRows) { row in
                BarMark(
                    x: .value("Actual spend", row.actualCostUSD ?? 0),
                    y: .value("Provider or model", row.label)
                )
                .foregroundStyle(
                    row.provider.caseInsensitiveCompare("Baseten") == .orderedSame
                        ? Color.basetenGreen
                        : Color.claudeTerracotta)
                .annotation(position: .trailing, alignment: .leading) {
                    Text(trafficCurrency(row.actualCostUSD))
                        .font(.caption.monospacedDigit())
                        .foregroundStyle(.secondary)
                }
            }
            .chartXAxis {
                AxisMarks(position: .bottom) { value in
                    AxisGridLine()
                    AxisTick()
                    AxisValueLabel {
                        if let amount = value.as(Double.self) {
                            Text(trafficCompactCurrency(amount))
                        }
                    }
                }
            }
            .chartXScale(domain: 0...chartMaximum)
            .animation(
                TrafficChartMotion.animation(reduceMotion: reduceMotion),
                value: pricedRows)
            .frame(height: max(110, CGFloat(pricedRows.count) * 38))
            .accessibilityLabel("Actual spend")
            .accessibilityValue(accessibilitySummary)
        }
    }

    private var accessibilitySummary: String {
        rows.map {
            "\($0.label), \(trafficCurrency($0.actualCostUSD))"
        }.joined(separator: "; ")
    }

    private var chartMaximum: Double {
        max(
            1,
            (pricedRows.compactMap(\.actualCostUSD).max() ?? 0) * 1.24)
    }
}

func trafficCurrency(_ value: Double?) -> String {
    guard let value else { return "No price" }
    return value.formatted(
        .currency(code: "USD")
            .precision(.fractionLength(2)))
}

func trafficCompactCurrency(_ value: Double) -> String {
    if value >= 1_000 {
        return "$" + (value / 1_000).formatted(
            .number.precision(.fractionLength(0...1))) + "k"
    }
    return "$" + value.formatted(
        .number.precision(.fractionLength(0)))
}

func trafficTokenCount(_ value: Int64) -> String {
    Double(value).formatted(
        .number
            .notation(.compactName)
            .precision(.fractionLength(0...1)))
}
