import SwiftUI

struct TrafficView: View {
    @ObservedObject var store: TrafficStore
    @State private var selectedTab: TrafficTab = .cost
    @State private var grouping: TrafficGrouping = .provider

    var body: some View {
        ScrollView {
            VStack(alignment: .leading, spacing: 20) {
                header
                status
                content
            }
            .padding(28)
            .frame(maxWidth: 900, alignment: .leading)
            .frame(maxWidth: .infinity, alignment: .topLeading)
        }
        .accessibilityIdentifier("traffic")
        .onAppear { store.start() }
        .onDisappear { store.stop() }
    }

    private var header: some View {
        VStack(alignment: .leading, spacing: 14) {
            HStack(alignment: .firstTextBaseline) {
                Label("Traffic", systemImage: "chart.bar.xaxis")
                    .font(.title2.weight(.semibold))
                Spacer()
                rangePicker
                Button {
                    store.requestRefresh()
                } label: {
                    if store.isRefreshing {
                        ProgressView()
                            .controlSize(.small)
                    } else {
                        Image(systemName: "arrow.clockwise")
                    }
                }
                .buttonStyle(.borderless)
                .disabled(store.isLoading || store.isRefreshing)
                .help("Refresh traffic data")
                .accessibilityLabel("Refresh traffic data")
            }

            HStack {
                Picker("View", selection: $selectedTab) {
                    ForEach(TrafficTab.allCases) { tab in
                        Text(tab.rawValue).tag(tab)
                    }
                }
                .pickerStyle(.segmented)
                .labelsHidden()
                .frame(width: 220)

                Spacer()

                Text("Group by")
                    .font(.caption)
                    .foregroundStyle(.secondary)
                Picker("Group by", selection: $grouping) {
                    ForEach(TrafficGrouping.allCases) { option in
                        Text(option.rawValue).tag(option)
                    }
                }
                .pickerStyle(.segmented)
                .labelsHidden()
                .frame(width: 180)
            }
        }
    }

    private var rangePicker: some View {
        Picker("Time range", selection: $store.range) {
            ForEach(TrafficRange.allCases) { range in
                Text(range.label).tag(range)
            }
        }
        .pickerStyle(.segmented)
        .labelsHidden()
        .frame(width: 150)
        .accessibilityLabel("Relative time range")
    }

    @ViewBuilder
    private var status: some View {
        if let snapshot = store.snapshot {
            if let message = trafficCollectionStatusMessage(
                snapshot.coverage) {
                trafficBanner(
                    message,
                    symbol: "pause.circle",
                    color: .secondary)
                    .accessibilityIdentifier(
                        "traffic-collection-paused")
            }
            if store.isStale {
                trafficBanner(
                    "Showing the last available traffic data. \(store.errorMessage ?? "Refresh failed.")",
                    symbol: "wifi.exclamationmark",
                    color: .orange)
            }
            if !snapshot.coverage.complete {
                trafficBanner(
                    partialHistoryMessage(snapshot.coverage),
                    symbol: "clock.badge.exclamationmark",
                    color: .orange)
            }
            if snapshot.coverage.unpricedActualCostRows > 0
                || snapshot.coverage.savingsUnpricedRows > 0 {
                trafficBanner(
                    pricingGapMessage(snapshot.coverage),
                    symbol: "dollarsign.circle",
                    color: .orange)
            }
            if let lastUpdated = store.lastUpdated {
                Text("Updated \(lastUpdated.formatted(.relative(presentation: .named)))")
                    .font(.caption)
                    .foregroundStyle(.secondary)
            }
        }
    }

    @ViewBuilder
    private var content: some View {
        if store.isLoading, store.snapshot == nil {
            VStack(spacing: 12) {
                ProgressView()
                Text("Loading traffic data…")
                    .foregroundStyle(.secondary)
            }
            .frame(maxWidth: .infinity, minHeight: 300)
        } else if let snapshot = store.snapshot {
            if snapshot.isEmpty {
                trafficEmptyState
            } else if selectedTab == .cost {
                TrafficCostView(snapshot: snapshot, grouping: grouping)
            } else {
                TrafficPerformanceView(
                    snapshot: snapshot,
                    grouping: grouping)
            }
        } else {
            trafficErrorState
        }
    }

    private var trafficEmptyState: some View {
        VStack(spacing: 12) {
            Image(systemName: "chart.bar.xaxis")
                .font(.system(size: 36))
                .foregroundStyle(.secondary)
            Text("No traffic in this range")
                .font(.title3.weight(.semibold))
            Text("Requests will appear here after Baseten Switch records eligible local traffic.")
                .multilineTextAlignment(.center)
                .foregroundStyle(.secondary)
                .frame(maxWidth: 420)
        }
        .frame(maxWidth: .infinity, minHeight: 300)
    }

    private var trafficErrorState: some View {
        VStack(spacing: 12) {
            Image(systemName: "exclamationmark.triangle")
                .font(.system(size: 34))
                .foregroundStyle(.orange)
            Text("Traffic is unavailable")
                .font(.title3.weight(.semibold))
            Text(store.errorMessage ?? "The gateway did not return traffic data.")
                .multilineTextAlignment(.center)
                .foregroundStyle(.secondary)
            Button("Try Again") {
                store.requestRefresh()
            }
        }
        .frame(maxWidth: .infinity, minHeight: 300)
    }

    private func partialHistoryMessage(_ coverage: TrafficCoverage) -> String {
        if coverage.reason.isEmpty {
            return "Partial history is available for the selected range."
        }
        return "Partial history: \(coverage.reason)"
    }

    private func pricingGapMessage(_ coverage: TrafficCoverage) -> String {
        var parts: [String] = []
        if coverage.unpricedActualCostRows > 0 {
            parts.append(
                "\(coverage.unpricedActualCostRows) actual-cost \(coverage.unpricedActualCostRows == 1 ? "row is" : "rows are") unpriced")
        }
        if coverage.savingsUnpricedRows > 0 {
            parts.append(
                "\(coverage.savingsUnpricedRows) savings \(coverage.savingsUnpricedRows == 1 ? "comparison is" : "comparisons are") unavailable")
        }
        return parts.joined(separator: "; ") + ". Known spend remains visible."
    }
}

func trafficCollectionStatusMessage(
    _ coverage: TrafficCoverage
) -> String? {
    guard !coverage.collectionEnabled else { return nil }
    return "Collection paused. Retained traffic history remains available."
}

private struct TrafficCostView: View {
    let snapshot: TrafficAnalyticsSnapshot
    let grouping: TrafficGrouping

    private var rows: [TrafficCostRow] {
        grouping == .provider
            ? snapshot.cost.providers
            : snapshot.cost.models
    }

    private var savingsGroups: [TrafficSavingsChartGroup] {
        if grouping == .provider {
            let summary = snapshot.cost.summary
            return [
                TrafficSavingsChartGroup(
                    id: "baseten",
                    label: "Baseten traffic",
                    actualBasetenCostUSD:
                        trafficSavingsEligibleBasetenCost(summary),
                    estimatedNativeCostUSD:
                        summary.estimatedNativeCostForBasetenUSD),
            ]
        }
        return snapshot.cost.savings.byBasetenModel.map {
            TrafficSavingsChartGroup(
                id: $0.id,
                label: $0.label,
                actualBasetenCostUSD: $0.actualBasetenCostUSD,
                estimatedNativeCostUSD: $0.estimatedNativeCostUSD)
        }
    }

    private var savingsRows: [TrafficSavingsTableRow] {
        if grouping == .provider {
            let summary = snapshot.cost.summary
            return [
                TrafficSavingsTableRow(
                    id: "baseten",
                    label: "Baseten",
                    requestedClaude: "All routed families",
                    actualBasetenCostUSD:
                        trafficSavingsEligibleBasetenCost(summary),
                    estimatedNativeCostUSD:
                        summary.estimatedNativeCostForBasetenUSD),
            ]
        }
        return snapshot.cost.savings.mappings.map {
            TrafficSavingsTableRow(
                id: $0.id,
                label: $0.label,
                requestedClaude: $0.requestedClaudeFamily,
                actualBasetenCostUSD: $0.actualBasetenCostUSD,
                estimatedNativeCostUSD: $0.estimatedNativeCostUSD)
        }
    }

    var body: some View {
        VStack(alignment: .leading, spacing: 24) {
            LazyVGrid(
                columns: Array(
                    repeating: GridItem(.flexible(), spacing: 12),
                    count: 3),
                spacing: 12
            ) {
                TrafficMetricCard(
                    title: "Actual Claude spend",
                    value: trafficCurrency(
                        snapshot.cost.summary.actualClaudeCostUSD),
                    detail: trafficTokenCount(
                        tokens(for: "Claude")),
                    brand: .claude)
                TrafficMetricCard(
                    title: "Actual Baseten spend",
                    value: trafficCurrency(
                        snapshot.cost.summary.actualBasetenCostUSD),
                    detail: trafficTokenCount(
                        tokens(for: "Baseten")),
                    brand: .baseten)
                TrafficMetricCard(
                    title: "Estimated savings",
                    value: trafficCurrency(
                        snapshot.cost.summary.savedUSD),
                    detail: snapshot.cost.summary.savedPercent.formatted(
                        .percent.scale(1)
                            .precision(.fractionLength(1))),
                    brand: .comparison)
            }

            TrafficSectionCard(
                title: "Actual spend",
                description:
                    "Observed spend for each provider’s own request set."
            ) {
                ActualSpendChart(rows: rows)
                TrafficActualSpendTable(
                    rows: rows,
                    collapsesLongLists: grouping == .model)
            }

            TrafficSectionCard(
                title: "Estimated savings",
                description:
                    "Baseten actual cost compared with the estimated Claude cost for the same routed traffic."
            ) {
                SavingsStackedChart(groups: savingsGroups)
                TrafficSavingsTable(
                    rows: savingsRows,
                    collapsesLongLists: grouping == .model)
                Label(
                    "Estimate holds observed input, output, and cache token counts constant. Tokenizers, generated output length, and cache behavior may differ between models.",
                    systemImage: "info.circle")
                    .font(.caption)
                    .foregroundStyle(.secondary)
                    .fixedSize(horizontal: false, vertical: true)
            }
        }
    }

    private func tokens(for provider: String) -> Int64 {
        snapshot.cost.providers.first {
            $0.provider.caseInsensitiveCompare(provider) == .orderedSame
        }?.tokens ?? 0
    }
}

private struct TrafficPerformanceView: View {
    let snapshot: TrafficAnalyticsSnapshot
    let grouping: TrafficGrouping

    private var rows: [TrafficPerformanceRow] {
        grouping == .provider
            ? snapshot.performance.providers
            : snapshot.performance.models
    }

    private var claude: TrafficPerformanceRow? {
        snapshot.performance.providers.first {
            $0.provider.caseInsensitiveCompare("Claude") == .orderedSame
        }
    }

    private var baseten: TrafficPerformanceRow? {
        snapshot.performance.providers.first {
            $0.provider.caseInsensitiveCompare("Baseten") == .orderedSame
        }
    }

    var body: some View {
        VStack(alignment: .leading, spacing: 24) {
            LazyVGrid(
                columns: Array(
                    repeating: GridItem(.flexible(), spacing: 12),
                    count: 3),
                spacing: 12
            ) {
                ForEach(
                    trafficPerformanceCardContents(
                        claude: claude,
                        baseten: baseten)
                ) { card in
                    TrafficMetricCard(
                        title: card.title,
                        value: card.value,
                        detail: card.detail,
                        brand: card.brand,
                        detailBrand: card.detailBrand)
                }
            }

            TrafficSectionCard(
                title: "Performance",
                description:
                    "Median latency and generation speed for requests with available measurements."
            ) {
                PerformanceCharts(rows: rows)
                TrafficPerformanceTable(
                    rows: rows,
                    collapsesLongLists: grouping == .model)
            }
        }
    }
}

struct TrafficPerformanceCardContent: Equatable, Identifiable {
    let title: String
    let value: String
    let detail: String
    let brand: TrafficBrand
    let detailBrand: TrafficBrand

    var id: String { title }
}

func trafficPerformanceCardContents(
    claude: TrafficPerformanceRow?,
    baseten: TrafficPerformanceRow?
) -> [TrafficPerformanceCardContent] {
    let basetenTTFT = baseten?.measuredMedianTTFTMS
    let claudeTTFT = claude?.measuredMedianTTFTMS
    let basetenTPS = baseten?.measuredMedianOutputTokensPerSecond
    let claudeTPS = claude?.measuredMedianOutputTokensPerSecond

    return [
        TrafficPerformanceCardContent(
            title: "Baseten median TTFT",
            value: trafficMilliseconds(basetenTTFT),
            detail: trafficComparisonDetail(
                value: "Claude \(trafficMilliseconds(claudeTTFT))",
                comparison: trafficPerformanceSpeedComparison(
                    baseten: basetenTTFT,
                    claude: claudeTTFT,
                    lowerIsBetter: true)),
            brand: .baseten,
            detailBrand: .claude),
        TrafficPerformanceCardContent(
            title: "Baseten median output speed",
            value: trafficTPS(basetenTPS),
            detail: trafficComparisonDetail(
                value: "Claude \(trafficTPS(claudeTPS))",
                comparison: trafficPerformanceSpeedComparison(
                    baseten: basetenTPS,
                    claude: claudeTPS,
                    lowerIsBetter: false)),
            brand: .baseten,
            detailBrand: .claude),
        TrafficPerformanceCardContent(
            title: "Baseten token volume",
            value: trafficTokenCount(baseten?.tokens ?? 0),
            detail:
                "Claude \(trafficTokenCount(claude?.tokens ?? 0)) tokens",
            brand: .baseten,
            detailBrand: .claude),
    ]
}

func trafficPerformanceSpeedComparison(
    baseten: Double?,
    claude: Double?,
    lowerIsBetter: Bool
) -> String? {
    guard let baseten, let claude,
          baseten.isFinite, claude.isFinite,
          baseten > 0, claude > 0 else {
        return nil
    }
    let ratio = lowerIsBetter
        ? claude / baseten
        : baseten / claude
    guard ratio.isFinite, ratio > 0 else { return nil }
    if abs(ratio - 1) < 0.05 {
        return "about the same"
    }
    if ratio > 1 {
        return ratio.formatted(
            .number.precision(.fractionLength(1))) + "× faster"
    }
    return (1 / ratio).formatted(
        .number.precision(.fractionLength(1))) + "× slower"
}

private func trafficComparisonDetail(
    value: String,
    comparison: String?
) -> String {
    guard let comparison else { return value }
    return "\(value) · \(comparison)"
}

private struct TrafficMetricCard: View {
    let title: String
    let value: String
    let detail: String
    let brand: TrafficBrand
    let detailBrand: TrafficBrand?

    init(
        title: String,
        value: String,
        detail: String,
        brand: TrafficBrand,
        detailBrand: TrafficBrand? = nil
    ) {
        self.title = title
        self.value = value
        self.detail = detail
        self.brand = brand
        self.detailBrand = detailBrand
    }

    var body: some View {
        VStack(alignment: .leading, spacing: 7) {
            HStack(spacing: 7) {
                TrafficBrandMark(brand: brand, size: 14)
                Text(title)
                    .font(.caption)
                    .foregroundStyle(.secondary)
            }
            Text(value)
                .font(.title2.weight(.semibold))
                .monospacedDigit()
            HStack(spacing: 5) {
                if let detailBrand {
                    TrafficBrandMark(brand: detailBrand, size: 12)
                }
                Text(detail)
                    .font(.caption)
                    .foregroundStyle(.secondary)
                    .lineLimit(1)
            }
        }
        .padding(14)
        .frame(maxWidth: .infinity, alignment: .leading)
        .background(
            Color.secondary.opacity(0.07),
            in: RoundedRectangle(cornerRadius: 10))
    }
}

private struct TrafficSectionCard<Content: View>: View {
    let title: String
    let description: String
    @ViewBuilder let content: Content

    init(
        title: String,
        description: String,
        @ViewBuilder content: () -> Content
    ) {
        self.title = title
        self.description = description
        self.content = content()
    }

    var body: some View {
        VStack(alignment: .leading, spacing: 16) {
            VStack(alignment: .leading, spacing: 3) {
                Text(title)
                    .font(.headline)
                Text(description)
                    .font(.caption)
                    .foregroundStyle(.secondary)
            }
            content
        }
        .padding(18)
        .background(
            RoundedRectangle(cornerRadius: 12)
                .fill(Color.secondary.opacity(0.045)))
        .overlay(
            RoundedRectangle(cornerRadius: 12)
                .stroke(Color.secondary.opacity(0.12), lineWidth: 1))
    }
}

private struct TrafficActualSpendTable: View {
    let rows: [TrafficCostRow]
    let collapsesLongLists: Bool
    @State private var isExpanded = false

    private var visibleRows: [TrafficCostRow] {
        Array(rows.prefix(
            trafficVisibleRowCount(
                totalCount: rows.count,
                isExpanded: isExpanded,
                collapsesLongLists: collapsesLongLists)))
    }

    var body: some View {
        VStack(alignment: .leading, spacing: 10) {
            VStack(spacing: 0) {
                HStack(spacing: 16) {
                    trafficTableHeader("Provider or model")
                        .frame(maxWidth: .infinity, alignment: .leading)
                    trafficTableHeader("Tokens")
                        .frame(maxWidth: .infinity, alignment: .trailing)
                    trafficTableHeader("Actual spend")
                        .frame(maxWidth: .infinity, alignment: .trailing)
                }
                .padding(.vertical, 7)

                Divider()

                ForEach(visibleRows.indices, id: \.self) { index in
                    let row = visibleRows[index]
                    HStack(alignment: .top, spacing: 16) {
                        TrafficProviderLabel(
                            provider: row.provider,
                            label: row.label)
                            .frame(
                                maxWidth: .infinity,
                                alignment: .leading)
                        Text(trafficTokenCount(row.tokens))
                            .monospacedDigit()
                            .frame(
                                maxWidth: .infinity,
                                alignment: .trailing)
                        VStack(alignment: .trailing, spacing: 1) {
                            Text(trafficCurrency(row.actualCostUSD))
                                .monospacedDigit()
                            if row.hasPartialCost {
                                Text(
                                    "Partial, \(row.unpricedRows) unpriced")
                                    .font(.caption2)
                                    .foregroundStyle(.orange)
                            }
                        }
                        .frame(
                            maxWidth: .infinity,
                            alignment: .trailing)
                    }
                    .padding(.vertical, 8)
                    .accessibilityElement(children: .combine)
                    .accessibilityLabel(
                        actualSpendAccessibilityLabel(row))

                    if index < visibleRows.count - 1 {
                        Divider()
                    }
                }
            }

            trafficExpansionButton(
                totalCount: rows.count,
                isExpanded: $isExpanded,
                collapsesLongLists: collapsesLongLists,
                identifier: "traffic-actual-spend-expand")
        }
    }

    private func actualSpendAccessibilityLabel(
        _ row: TrafficCostRow
    ) -> String {
        var label =
            "\(row.label), \(trafficTokenCount(row.tokens)) tokens, "
            + "actual spend \(trafficCurrency(row.actualCostUSD))"
        if row.hasPartialCost {
            label += ", partial cost, \(row.unpricedRows) unpriced"
        }
        return label
    }
}

private struct TrafficSavingsTableRow: Identifiable {
    let id: String
    let label: String
    let requestedClaude: String
    let actualBasetenCostUSD: Double
    let estimatedNativeCostUSD: Double

    var savedUSD: Double {
        estimatedNativeCostUSD - actualBasetenCostUSD
    }
    var savedPercent: Double? {
        guard estimatedNativeCostUSD > 0 else { return nil }
        return savedUSD / estimatedNativeCostUSD
    }
}

private struct TrafficSavingsTable: View {
    let rows: [TrafficSavingsTableRow]
    let collapsesLongLists: Bool
    @State private var isExpanded = false

    private var visibleRows: [TrafficSavingsTableRow] {
        Array(rows.prefix(
            trafficVisibleRowCount(
                totalCount: rows.count,
                isExpanded: isExpanded,
                collapsesLongLists: collapsesLongLists)))
    }

    var body: some View {
        VStack(alignment: .leading, spacing: 10) {
            ViewThatFits(in: .horizontal) {
                wideSavingsTable
                    .frame(minWidth: 790)
                compactSavingsTable
            }

            trafficExpansionButton(
                totalCount: rows.count,
                isExpanded: $isExpanded,
                collapsesLongLists: collapsesLongLists,
                identifier: "traffic-savings-expand")
        }
    }

    private var wideSavingsTable: some View {
        VStack(spacing: 0) {
            HStack(spacing: 16) {
                trafficTableHeader("Baseten model")
                    .frame(width: 160, alignment: .leading)
                trafficTableHeader("Requested Claude")
                    .frame(width: 150, alignment: .leading)
                trafficTableHeader("Baseten actual")
                    .frame(width: 110, alignment: .trailing)
                trafficTableHeader("Native estimate")
                    .frame(width: 110, alignment: .trailing)
                trafficTableHeader("Savings")
                    .frame(width: 100, alignment: .trailing)
                trafficTableHeader("Saved")
                    .frame(width: 80, alignment: .trailing)
            }
            .padding(.vertical, 7)

            Divider()

            ForEach(visibleRows.indices, id: \.self) { index in
                let row = visibleRows[index]
                HStack(alignment: .top, spacing: 16) {
                    TrafficProviderLabel(
                        provider: "Baseten",
                        label: row.label)
                        .frame(width: 160, alignment: .leading)
                    Text(row.requestedClaude)
                        .frame(width: 150, alignment: .leading)
                    Text(
                        trafficCurrency(
                            row.actualBasetenCostUSD))
                        .monospacedDigit()
                        .frame(width: 110, alignment: .trailing)
                    Text(
                        trafficCurrency(
                            row.estimatedNativeCostUSD))
                        .monospacedDigit()
                        .frame(width: 110, alignment: .trailing)
                    Text(trafficCurrency(row.savedUSD))
                        .monospacedDigit()
                        .frame(width: 100, alignment: .trailing)
                    Text(savedPercent(row))
                        .monospacedDigit()
                        .frame(width: 80, alignment: .trailing)
                }
                .padding(.vertical, 8)
                .accessibilityElement(children: .combine)
                .accessibilityLabel(
                    savingsAccessibilityLabel(row))

                if index < visibleRows.count - 1 {
                    Divider()
                }
            }
        }
    }

    private var compactSavingsTable: some View {
        VStack(spacing: 0) {
            ForEach(visibleRows.indices, id: \.self) { index in
                let row = visibleRows[index]
                VStack(alignment: .leading, spacing: 9) {
                    TrafficProviderLabel(
                        provider: "Baseten",
                        label: row.label)
                        .font(.body.weight(.medium))

                    Grid(
                        alignment: .leading,
                        horizontalSpacing: 16,
                        verticalSpacing: 5
                    ) {
                        compactSavingsField(
                            label: "Requested Claude",
                            value: row.requestedClaude)
                        compactSavingsField(
                            label: "Baseten actual",
                            value: trafficCurrency(
                                row.actualBasetenCostUSD))
                        compactSavingsField(
                            label: "Native estimate",
                            value: trafficCurrency(
                                row.estimatedNativeCostUSD))
                        compactSavingsField(
                            label: "Savings",
                            value: trafficCurrency(row.savedUSD))
                        compactSavingsField(
                            label: "Saved",
                            value: savedPercent(row))
                    }
                    .monospacedDigit()
                }
                .padding(.vertical, 10)
                .accessibilityElement(children: .combine)
                .accessibilityLabel(
                    savingsAccessibilityLabel(row))

                if index < visibleRows.count - 1 {
                    Divider()
                }
            }
        }
    }

    private func compactSavingsField(
        label: String,
        value: String
    ) -> some View {
        GridRow {
            Text(label)
                .font(.caption)
                .foregroundStyle(.secondary)
            Text(value)
                .frame(maxWidth: .infinity, alignment: .trailing)
        }
    }

    private func savedPercent(_ row: TrafficSavingsTableRow) -> String {
        row.savedPercent?.formatted(
            .percent.precision(.fractionLength(1))) ?? "N/A"
    }

    private func savingsAccessibilityLabel(
        _ row: TrafficSavingsTableRow
    ) -> String {
        let saved = row.savedPercent == nil
            ? "unavailable"
            : savedPercent(row)
        return
            "\(row.label), requested Claude \(row.requestedClaude), "
            + "Baseten actual \(trafficCurrency(row.actualBasetenCostUSD)), "
            + "native estimate "
            + "\(trafficCurrency(row.estimatedNativeCostUSD)), "
            + "savings \(trafficCurrency(row.savedUSD)), saved \(saved)"
    }
}

private struct TrafficPerformanceTable: View {
    let rows: [TrafficPerformanceRow]
    let collapsesLongLists: Bool
    @State private var isExpanded = false

    private var visibleRows: [TrafficPerformanceRow] {
        Array(rows.prefix(
            trafficVisibleRowCount(
                totalCount: rows.count,
                isExpanded: isExpanded,
                collapsesLongLists: collapsesLongLists)))
    }

    var body: some View {
        VStack(alignment: .leading, spacing: 10) {
            VStack(spacing: 0) {
                HStack(spacing: 16) {
                    trafficTableHeader("Provider or model")
                        .frame(maxWidth: .infinity, alignment: .leading)
                    trafficTableHeader("Tokens")
                        .frame(maxWidth: .infinity, alignment: .trailing)
                    trafficTableHeader("Median TTFT")
                        .frame(maxWidth: .infinity, alignment: .trailing)
                    trafficTableHeader("Median output TPS")
                        .frame(maxWidth: .infinity, alignment: .trailing)
                }
                .padding(.vertical, 7)

                Divider()

                ForEach(visibleRows.indices, id: \.self) { index in
                    let row = visibleRows[index]
                    HStack(alignment: .top, spacing: 16) {
                        TrafficProviderLabel(
                            provider: row.provider,
                            label: row.label)
                            .frame(
                                maxWidth: .infinity,
                                alignment: .leading)
                        Text(trafficTokenCount(row.tokens))
                            .monospacedDigit()
                            .frame(
                                maxWidth: .infinity,
                                alignment: .trailing)
                        Text(
                            trafficMilliseconds(
                                row.measuredMedianTTFTMS))
                            .monospacedDigit()
                            .frame(
                                maxWidth: .infinity,
                                alignment: .trailing)
                        Text(
                            trafficTPS(
                                row.measuredMedianOutputTokensPerSecond))
                            .monospacedDigit()
                            .frame(
                                maxWidth: .infinity,
                                alignment: .trailing)
                    }
                    .padding(.vertical, 8)
                    .accessibilityElement(children: .combine)
                    .accessibilityLabel(
                        performanceAccessibilityLabel(row))

                    if index < visibleRows.count - 1 {
                        Divider()
                    }
                }
            }

            trafficExpansionButton(
                totalCount: rows.count,
                isExpanded: $isExpanded,
                collapsesLongLists: collapsesLongLists,
                identifier: "traffic-performance-expand")
        }
    }

    private func performanceAccessibilityLabel(
        _ row: TrafficPerformanceRow
    ) -> String {
        return
            "\(row.label), \(trafficTokenCount(row.tokens)) tokens, "
            + "median TTFT "
            + "\(trafficMilliseconds(row.measuredMedianTTFTMS)), "
            + "median output speed "
            + "\(trafficTPS(row.measuredMedianOutputTokensPerSecond))"
    }
}

private let trafficCollapsedTableRowLimit = 8

func trafficVisibleRowCount(
    totalCount: Int,
    isExpanded: Bool,
    collapsesLongLists: Bool
) -> Int {
    guard collapsesLongLists, !isExpanded else {
        return max(0, totalCount)
    }
    return min(max(0, totalCount), trafficCollapsedTableRowLimit)
}

func trafficExpansionButtonTitle(
    totalCount: Int,
    isExpanded: Bool,
    collapsesLongLists: Bool
) -> String? {
    guard collapsesLongLists,
          totalCount > trafficCollapsedTableRowLimit else {
        return nil
    }
    if isExpanded {
        return "Show less"
    }
    return "Show \(totalCount - trafficCollapsedTableRowLimit) more"
}

@ViewBuilder
private func trafficExpansionButton(
    totalCount: Int,
    isExpanded: Binding<Bool>,
    collapsesLongLists: Bool,
    identifier: String
) -> some View {
    if let title = trafficExpansionButtonTitle(
        totalCount: totalCount,
        isExpanded: isExpanded.wrappedValue,
        collapsesLongLists: collapsesLongLists
    ) {
        Button(title) {
            isExpanded.wrappedValue.toggle()
        }
        .buttonStyle(.borderless)
        .accessibilityIdentifier(identifier)
    }
}

private func trafficTableHeader(_ title: String) -> some View {
    Text(title)
        .font(.caption.weight(.semibold))
        .foregroundStyle(.secondary)
        .accessibilityAddTraits(.isHeader)
}

private struct TrafficProviderLabel: View {
    let provider: String
    let label: String

    var body: some View {
        HStack(spacing: 7) {
            TrafficBrandMark(
                brand: provider.caseInsensitiveCompare("Baseten")
                    == .orderedSame
                    ? .baseten
                    : .claude,
                size: 14)
            Text(label)
        }
    }
}

private func trafficBanner(
    _ text: String,
    symbol: String,
    color: Color
) -> some View {
    HStack(alignment: .top, spacing: 9) {
        Image(systemName: symbol)
            .foregroundStyle(color)
        Text(text)
            .font(.callout)
            .fixedSize(horizontal: false, vertical: true)
        Spacer()
    }
    .padding(11)
    .background(color.opacity(0.1), in: RoundedRectangle(cornerRadius: 9))
}

private func trafficMilliseconds(_ value: Double?) -> String {
    guard let value else { return "No data" }
    return value.formatted(
        .number.precision(.fractionLength(0...1))) + " ms"
}

private func trafficTPS(_ value: Double?) -> String {
    guard let value else { return "No data" }
    return value.formatted(
        .number.precision(.fractionLength(0...1))) + " tok/s"
}
