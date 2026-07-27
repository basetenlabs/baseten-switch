import SwiftUI

enum TrafficBrand: Equatable {
    case claude
    case baseten
    case comparison
}

struct TrafficBrandMark: View {
    let brand: TrafficBrand
    var size: CGFloat = 16

    var body: some View {
        switch brand {
        case .claude:
            ClaudeBrandShape()
                .fill(Color.claudeTerracotta)
                .frame(width: size, height: size)
                .accessibilityHidden(true)
        case .baseten:
            BasetenBrandShape()
                .fill(Color.basetenGreen)
                .frame(width: size, height: size)
                .accessibilityHidden(true)
        case .comparison:
            HStack(spacing: 3) {
                BasetenBrandShape()
                    .fill(Color.basetenGreen)
                    .frame(width: size, height: size)
                ClaudeBrandShape()
                    .fill(Color.claudeTerracotta)
                    .frame(width: size, height: size)
            }
            .accessibilityHidden(true)
        }
    }
}

/// The Baseten branch glyph, normalized from the bundled brand SVG.
private struct BasetenBrandShape: Shape {
    func path(in rect: CGRect) -> Path {
        let source = CGRect(x: 278.919, y: 213, width: 523.162, height: 654)
        let scale = min(rect.width / source.width, rect.height / source.height)
        let xOffset = rect.midX - source.midX * scale
        let yOffset = rect.midY - source.midY * scale
        func point(_ x: CGFloat, _ y: CGFloat) -> CGPoint {
            CGPoint(x: x * scale + xOffset, y: y * scale + yOffset)
        }

        var path = Path()
        path.move(to: point(287.109, 343.786))
        path.addLine(to: point(671.273, 343.786))
        path.addLine(to: point(671.273, 474.572))
        path.addLine(to: point(417.883, 474.572))
        path.addQuadCurve(
            to: point(409.692, 482.764),
            control: point(409.692, 474.572))
        path.addLine(to: point(409.692, 597.201))
        path.addQuadCurve(
            to: point(417.883, 605.393),
            control: point(409.692, 605.393))
        path.addLine(to: point(671.273, 605.393))
        path.addLine(to: point(671.273, 736.179))
        path.addLine(to: point(548.656, 736.179))
        path.addQuadCurve(
            to: point(540.465, 744.371),
            control: point(540.465, 736.179))
        path.addLine(to: point(540.465, 858.808))
        path.addQuadCurve(
            to: point(548.656, 867),
            control: point(540.465, 867))
        path.addLine(to: point(663.082, 867))
        path.addQuadCurve(
            to: point(671.273, 858.808),
            control: point(671.273, 867))
        path.addLine(to: point(671.273, 736.179))
        path.addLine(to: point(793.89, 736.179))
        path.addQuadCurve(
            to: point(802.081, 727.987),
            control: point(802.081, 736.179))
        path.addLine(to: point(802.081, 613.55))
        path.addQuadCurve(
            to: point(793.89, 605.358),
            control: point(802.081, 605.358))
        path.addLine(to: point(671.273, 605.358))
        path.addLine(to: point(671.273, 474.572))
        path.addLine(to: point(793.89, 474.572))
        path.addQuadCurve(
            to: point(802.081, 466.38),
            control: point(802.081, 474.572))
        path.addLine(to: point(802.081, 351.943))
        path.addQuadCurve(
            to: point(793.89, 343.751),
            control: point(802.081, 343.751))
        path.addLine(to: point(671.273, 343.751))
        path.addLine(to: point(671.273, 221.192))
        path.addQuadCurve(
            to: point(663.082, 213),
            control: point(671.273, 213))
        path.addLine(to: point(287.109, 213))
        path.addQuadCurve(
            to: point(278.919, 221.192),
            control: point(278.919, 213))
        path.addLine(to: point(278.919, 335.629))
        path.addQuadCurve(
            to: point(287.109, 343.821),
            control: point(278.919, 343.821))
        path.closeSubpath()
        return path
    }
}

/// Official Claude Spark geometry from Anthropic's media press kit:
/// https://www.anthropic.com/press-kit
struct ClaudeBrandShape: Shape {
    static let sourceViewBox = CGRect(
        x: 0,
        y: 0,
        width: 94,
        height: 94)

    private static let leadingLinePoints: [CGPoint] = [
        CGPoint(x: 37.1822, y: 52.1167),
        CGPoint(x: 37.4857, y: 51.2122),
        CGPoint(x: 37.1822, y: 50.7085),
        CGPoint(x: 36.2715, y: 50.7085),
        CGPoint(x: 33.1852, y: 50.5208),
        CGPoint(x: 22.6615, y: 50.2391),
        CGPoint(x: 13.5545, y: 49.8636),
        CGPoint(x: 4.70044, y: 49.3942),
        CGPoint(x: 2.47428, y: 48.9248),
        CGPoint(x: 0.399902, y: 46.1553),
        CGPoint(x: 0.602281, y: 44.794),
        CGPoint(x: 2.47428, y: 43.5266),
        CGPoint(x: 5.15579, y: 43.7613),
        CGPoint(x: 11.0754, y: 44.1837),
        CGPoint(x: 19.98, y: 44.794),
        CGPoint(x: 26.4055, y: 45.1695),
        CGPoint(x: 35.9679, y: 46.1553),
        CGPoint(x: 37.4857, y: 46.1553),
        CGPoint(x: 37.6881, y: 45.545),
        CGPoint(x: 37.1822, y: 45.1695),
        CGPoint(x: 36.7774, y: 44.794),
        CGPoint(x: 27.5692, y: 38.5508),
        CGPoint(x: 17.6021, y: 31.9791),
        CGPoint(x: 12.3908, y: 28.1769),
        CGPoint(x: 9.60812, y: 26.2524),
        CGPoint(x: 8.19147, y: 24.4686),
        CGPoint(x: 7.58433, y: 20.5256),
        CGPoint(x: 10.1141, y: 17.7091),
        CGPoint(x: 13.5545, y: 17.9438),
        CGPoint(x: 14.4146, y: 18.1785),
        CGPoint(x: 17.9056, y: 20.8542),
        CGPoint(x: 25.343, y: 26.6279),
        CGPoint(x: 35.0572, y: 33.7629),
        CGPoint(x: 36.4739, y: 34.9364),
        CGPoint(x: 37.0443, y: 34.5514),
        CGPoint(x: 37.1316, y: 34.2792),
        CGPoint(x: 36.4739, y: 33.1996),
        CGPoint(x: 31.212, y: 23.6706),
        CGPoint(x: 25.596, y: 13.9539),
        CGPoint(x: 23.0663, y: 9.91695),
        CGPoint(x: 22.4086, y: 7.52296),
    ]

    private static let trailingLinePoints: [CGPoint] = [
        CGPoint(x: 24.8877, y: 0.716544),
        CGPoint(x: 26.5067, y: 0.200195),
        CGPoint(x: 30.4025, y: 0.716544),
        CGPoint(x: 32.0215, y: 2.12477),
        CGPoint(x: 34.4501, y: 7.66379),
        CGPoint(x: 38.3458, y: 16.3478),
        CGPoint(x: 44.4172, y: 28.1769),
        CGPoint(x: 46.188, y: 31.6975),
        CGPoint(x: 47.1493, y: 34.9364),
        CGPoint(x: 47.5035, y: 35.9222),
        CGPoint(x: 48.1106, y: 35.9222),
        CGPoint(x: 48.1106, y: 35.3589),
        CGPoint(x: 48.6166, y: 28.6933),
        CGPoint(x: 49.5273, y: 20.5256),
        CGPoint(x: 50.438, y: 10.0108),
        CGPoint(x: 50.7415, y: 7.05356),
        CGPoint(x: 52.2088, y: 3.48605),
        CGPoint(x: 55.1433, y: 1.56148),
        CGPoint(x: 57.42, y: 2.64112),
        CGPoint(x: 59.292, y: 5.31674),
        CGPoint(x: 59.039, y: 7.05356),
        CGPoint(x: 57.926, y: 14.2824),
        CGPoint(x: 55.7504, y: 25.5952),
        CGPoint(x: 54.3337, y: 33.1996),
        CGPoint(x: 55.1433, y: 33.1996),
        CGPoint(x: 56.1046, y: 32.2138),
        CGPoint(x: 59.9497, y: 27.1442),
        CGPoint(x: 66.3752, y: 19.0704),
        CGPoint(x: 69.2085, y: 15.8784),
        CGPoint(x: 72.5478, y: 12.3579),
        CGPoint(x: 74.6728, y: 10.668),
        CGPoint(x: 78.7203, y: 10.668),
        CGPoint(x: 81.6548, y: 15.0804),
        CGPoint(x: 80.3394, y: 19.6337),
        CGPoint(x: 76.1906, y: 24.8911),
        CGPoint(x: 72.7502, y: 29.3504),
        CGPoint(x: 67.8172, y: 35.9595),
        CGPoint(x: 64.7562, y: 41.2734),
        CGPoint(x: 65.0307, y: 41.7118),
        CGPoint(x: 65.7681, y: 41.6489),
        CGPoint(x: 76.8989, y: 39.255),
        CGPoint(x: 82.9197, y: 38.1753),
        CGPoint(x: 90.1041, y: 36.9549),
        CGPoint(x: 93.3422, y: 38.457),
        CGPoint(x: 93.6963, y: 40.006),
        CGPoint(x: 92.4315, y: 43.151),
        CGPoint(x: 84.7411, y: 45.0287),
        CGPoint(x: 75.7353, y: 46.8594),
        CGPoint(x: 62.3244, y: 50.0164),
        CGPoint(x: 62.1759, y: 50.1358),
        CGPoint(x: 62.3512, y: 50.3958),
        CGPoint(x: 68.399, y: 50.9432),
        CGPoint(x: 70.9794, y: 51.084),
        CGPoint(x: 77.3037, y: 51.084),
        CGPoint(x: 89.0922, y: 51.9759),
        CGPoint(x: 92.1785, y: 53.9944),
        CGPoint(x: 93.9999, y: 56.4822),
        CGPoint(x: 93.6963, y: 58.4068),
        CGPoint(x: 88.9404, y: 60.8008),
        CGPoint(x: 82.5655, y: 59.2987),
        CGPoint(x: 67.6401, y: 55.7312),
        CGPoint(x: 62.5301, y: 54.4638),
        CGPoint(x: 61.8217, y: 54.4638),
        CGPoint(x: 61.8217, y: 54.8862),
        CGPoint(x: 66.0717, y: 59.064),
        CGPoint(x: 73.9139, y: 66.1051),
        CGPoint(x: 83.6786, y: 75.2116),
        CGPoint(x: 84.1845, y: 77.4648),
        CGPoint(x: 82.9197, y: 79.2485),
        CGPoint(x: 81.6042, y: 79.0608),
        CGPoint(x: 73.0032, y: 72.5829),
        CGPoint(x: 69.6639, y: 69.6726),
        CGPoint(x: 62.1759, y: 63.3356),
        CGPoint(x: 61.67, y: 63.3356),
        CGPoint(x: 61.67, y: 63.9928),
        CGPoint(x: 63.3902, y: 66.5276),
        CGPoint(x: 72.5478, y: 80.2812),
        CGPoint(x: 73.0032, y: 84.5059),
        CGPoint(x: 72.3454, y: 85.8672),
        CGPoint(x: 69.9675, y: 86.7121),
        CGPoint(x: 67.3871, y: 86.2427),
        CGPoint(x: 61.9735, y: 78.6852),
        CGPoint(x: 56.4587, y: 70.2359),
        CGPoint(x: 52.0064, y: 62.6315),
        CGPoint(x: 51.4687, y: 62.971),
        CGPoint(x: 48.8189, y: 91.2654),
        CGPoint(x: 47.6047, y: 92.7206),
        CGPoint(x: 44.7714, y: 93.8002),
        CGPoint(x: 42.3934, y: 92.0164),
        CGPoint(x: 41.1286, y: 89.1061),
        CGPoint(x: 42.3934, y: 83.3324),
        CGPoint(x: 43.9113, y: 75.8219),
        CGPoint(x: 45.1255, y: 69.8604),
        CGPoint(x: 46.2386, y: 62.4437),
        CGPoint(x: 46.9184, y: 59.9661),
        CGPoint(x: 46.8583, y: 59.8003),
        CGPoint(x: 46.3153, y: 59.8916),
        CGPoint(x: 40.7238, y: 67.5603),
        CGPoint(x: 32.2239, y: 79.0608),
        CGPoint(x: 25.4948, y: 86.2427),
        CGPoint(x: 23.8758, y: 86.8999),
        CGPoint(x: 21.0931, y: 85.4447),
        CGPoint(x: 21.3461, y: 82.863),
        CGPoint(x: 22.9145, y: 80.5629),
        CGPoint(x: 32.2239, y: 68.7338),
        CGPoint(x: 37.8399, y: 61.3641),
        CGPoint(x: 41.4594, y: 57.1337),
        CGPoint(x: 41.4242, y: 56.5218),
        CGPoint(x: 41.2244, y: 56.5048),
        CGPoint(x: 16.489, y: 72.6299),
        CGPoint(x: 12.0873, y: 73.1932),
        CGPoint(x: 10.1647, y: 71.4094),
        CGPoint(x: 10.4176, y: 68.4991),
        CGPoint(x: 11.3283, y: 67.5603),
        CGPoint(x: 18.7657, y: 62.4437),
    ]

    func path(in rect: CGRect) -> Path {
        let source = Self.sourceViewBox
        let scale = min(rect.width / source.width, rect.height / source.height)
        let xOffset = rect.midX - source.midX * scale
        let yOffset = rect.midY - source.midY * scale
        func point(_ sourcePoint: CGPoint) -> CGPoint {
            CGPoint(
                x: sourcePoint.x * scale + xOffset,
                y: sourcePoint.y * scale + yOffset)
        }

        var path = Path()
        path.move(to: point(CGPoint(x: 18.7657, y: 62.4437)))
        for sourcePoint in Self.leadingLinePoints {
            path.addLine(to: point(sourcePoint))
        }
        path.addCurve(
            to: point(CGPoint(x: 22.0038, y: 4.65957)),
            control1: point(CGPoint(x: 22.1538, y: 6.51831)),
            control2: point(CGPoint(x: 22.0038, y: 5.68714)))
        for sourcePoint in Self.trailingLinePoints {
            path.addLine(to: point(sourcePoint))
        }
        path.closeSubpath()
        return path
    }
}
