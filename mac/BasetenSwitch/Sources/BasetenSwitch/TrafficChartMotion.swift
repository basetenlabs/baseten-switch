import SwiftUI

enum TrafficChartMotion {
    static let duration: TimeInterval = 0.45

    static func animation(reduceMotion: Bool) -> Animation? {
        guard !reduceMotion else { return nil }
        return .easeInOut(duration: duration)
    }
}
