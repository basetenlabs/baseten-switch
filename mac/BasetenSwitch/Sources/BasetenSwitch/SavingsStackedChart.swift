import Charts
import SwiftUI

struct SavingsStackedChart: View {
    let groups: [TrafficSavingsChartGroup]
    @Environment(\.accessibilityReduceMotion) private var reduceMotion

    private var hasNegativeSavings: Bool {
        groups.contains { $0.hasNegativeSavings }
    }

    var body: some View {
        VStack(alignment: .leading, spacing: 12) {
            HStack(spacing: 18) {
                legend(
                    color: .basetenGreen,
                    opacity: 1,
                    title: "Baseten actual")
                legend(
                    color: .claudeTerracotta,
                    opacity: 0.42,
                    title: "Additional Claude estimate")
                if hasNegativeSavings {
                    legend(
                        color: .claudeTerracotta,
                        opacity: 1,
                        title: "Lower Claude estimate marker")
                }
            }
            .font(.caption)
            .foregroundStyle(.secondary)

            Chart {
                ForEach(groups) { group in
                    BarMark(
                        x: .value("Cost", group.actualBasetenCostUSD),
                        y: .value("Traffic", group.label),
                        stacking: .standard
                    )
                    .foregroundStyle(Color.basetenGreen)

                    if group.estimatedAdditionalClaudeCostUSD > 0 {
                        BarMark(
                            x: .value(
                                "Additional Claude estimate",
                                group.estimatedAdditionalClaudeCostUSD),
                            y: .value("Traffic", group.label),
                            stacking: .standard
                        )
                        .foregroundStyle(Color.claudeTerracotta)
                        .opacity(0.42)
                    }

                    if group.hasNegativeSavings {
                        PointMark(
                            x: .value(
                                "Estimated Claude total",
                                group.estimatedNativeCostUSD),
                            y: .value("Traffic", group.label)
                        )
                        .foregroundStyle(Color.claudeTerracotta)
                        .symbol(.diamond)
                        .symbolSize(70)
                    }
                }
            }
            .animation(
                TrafficChartMotion.animation(reduceMotion: reduceMotion),
                value: groups)
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
            .frame(height: max(100, CGFloat(groups.count) * 44))
            .accessibilityLabel(
                "Actual Baseten cost and estimated additional Claude cost")
            .accessibilityValue(accessibilitySummary)

            ForEach(groups) { group in
                VStack(alignment: .leading, spacing: 2) {
                    HStack {
                        Text(group.label)
                            .lineLimit(1)
                        Spacer()
                        Text(
                            "\(trafficCurrency(group.actualBasetenCostUSD)) actual · \(trafficCurrency(group.estimatedNativeCostUSD)) estimated Claude total")
                            .font(.caption.monospacedDigit())
                            .foregroundStyle(.secondary)
                    }
                    if group.hasNegativeSavings {
                        Text(
                            "Baseten cost \(trafficCurrency(group.actualBasetenCostUSD - group.estimatedNativeCostUSD)) more than the Claude estimate.")
                            .font(.caption)
                            .foregroundStyle(.orange)
                    }
                }
            }
        }
    }

    private func legend(
        color: Color,
        opacity: Double,
        title: String
    ) -> some View {
        HStack(spacing: 6) {
            RoundedRectangle(cornerRadius: 2)
                .fill(color.opacity(opacity))
                .frame(width: 18, height: 8)
            Text(title)
        }
    }

    private var accessibilitySummary: String {
        groups.map {
            "\($0.label), \(trafficCurrency($0.actualBasetenCostUSD)) actual Baseten, \(trafficCurrency($0.estimatedNativeCostUSD)) estimated Claude total"
        }.joined(separator: "; ")
    }
}
