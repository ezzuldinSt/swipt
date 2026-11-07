// The compute shaders of a GPU-resident frame (gpu_windows.go). They read the AYUV texture a
// frame was copied into through an R8G8B8A8_UINT view, which maps V, U, Y and A to R, G, B and
// A, and do in integers what picture.go does on the CPU, so their output is the same to the
// bit: coefficients in 16.16 fixed point, rounded averages, the same clamping and gray weights.

Texture2D<uint4> source : register(t0);
Texture2D<uint4> other : register(t1);
RWByteAddressBuffer target : register(u0);

cbuffer Parameters : register(b0) {
	// The source pixel of output (x, y) is origin + x·across + y·down (picture.go turning).
	int4 turning;  // origin x, origin y, across x, across y
	int4 turning2; // down x, down y, threads a row of the dispatch, 0
	// The rectangle of the upright output that is read: its corner and its size.
	int4 region;
	// gray and averages: the plane's width and height; compare: the rows of a band.
	int4 plane;
	int4 coefficients;  // luma offset, luma scale, Cr to red, Cb to green
	int4 coefficients2; // Cr to green, Cb to blue, general, 0
	int4 offsets;       // the general form's offsets of Y, Cb and Cr
	int4 matrix0;       // the general form's rows: red, green, blue
	int4 matrix1;
	int4 matrix2;
};

uint channel(int value) {
	return (uint)clamp(value >> 16, 0, 255);
}

// rgbOf converts Y, Cb and Cr samples, or averages of them, as coefficients.rgb does.
uint3 rgbOf(int luma, int cb, int cr) {
	if (coefficients2.z != 0) {
		luma -= offsets.x;
		cb -= offsets.y;
		cr -= offsets.z;
		return uint3(channel(luma * matrix0.x + cb * matrix0.y + cr * matrix0.z + 32768),
			channel(luma * matrix1.x + cb * matrix1.y + cr * matrix1.z + 32768),
			channel(luma * matrix2.x + cb * matrix2.y + cr * matrix2.z + 32768));
	}
	int scaled = (luma - coefficients.x) * coefficients.y + 32768;
	cb -= 128;
	cr -= 128;
	return uint3(channel(scaled + cr * coefficients.z), channel(scaled - cb * coefficients.w - cr * coefficients2.x),
		channel(scaled + cb * coefficients2.y));
}

// convert writes the RGBA of the region, region.z·4 bytes a row.
[numthreads(16, 16, 1)]
void convert(uint3 id : SV_DispatchThreadID) {
	int x = id.x, y = id.y;
	if (x >= region.z || y >= region.w) {
		return;
	}
	int outX = region.x + x, outY = region.y + y;
	int sourceX = turning.x + outX * turning.z + outY * turning2.x;
	int sourceY = turning.y + outX * turning.w + outY * turning2.y;
	uint4 pixel = source.Load(int3(sourceX, sourceY, 0));
	uint3 color = rgbOf(pixel.z, pixel.y, pixel.x);
	target.Store((y * region.z + x) * 4, color.x | color.y << 8 | color.z << 16 | 0xff000000u);
}

// average is the color of the region's block of plane pixel (qx, qy), averaged in Y, Cb and Cr
// (picture.go grayInto), black for an empty block.
uint3 average(int qx, int qy) {
	int top = region.y + qy * region.w / plane.y, bottom = region.y + (qy + 1) * region.w / plane.y;
	int left = region.x + qx * region.z / plane.x, right = region.x + (qx + 1) * region.z / plane.x;
	int luma = 0, cb = 0, cr = 0;
	for (int y = top; y < bottom; y++) {
		for (int x = left; x < right; x++) {
			uint4 pixel = source.Load(int3(x, y, 0));
			cr += pixel.x;
			cb += pixel.y;
			luma += pixel.z;
		}
	}
	int count = (bottom - top) * (right - left);
	if (count == 0) {
		return uint3(0, 0, 0);
	}
	return rgbOf((luma + count / 2) / count, (cb + count / 2) / count, (cr + count / 2) / count);
}

// gray writes the plane in gray, plane.x bytes a row with no padding: each thread four pixels
// of it in a row.
[numthreads(64, 1, 1)]
void gray(uint3 id : SV_DispatchThreadID) {
	int total = plane.x * plane.y;
	int first = (id.y * turning2.z + id.x) * 4;
	if (first >= total) {
		return;
	}
	uint word = 0;
	for (int k = 0; k < 4 && first + k < total; k++) {
		int index = first + k;
		uint3 color = average(index % plane.x, index / plane.x);
		word |= ((77 * color.x + 150 * color.y + 29 * color.z) >> 8) << (8 * k);
	}
	target.Store(first, word);
}

// averages writes the plane's averaged colors as RGBA, which the CPU then takes from wide-gamut
// primaries to sRGB before it grays them.
[numthreads(16, 16, 1)]
void averages(uint3 id : SV_DispatchThreadID) {
	int qx = id.x, qy = id.y;
	if (qx >= plane.x || qy >= plane.y) {
		return;
	}
	uint3 color = average(qx, qy);
	target.Store((qy * plane.x + qx) * 4, color.x | color.y << 8 | color.z << 16 | 0xff000000u);
}

groupshared uint differing;

// compare sets bit b of the first word when a sample of band b, the region's rows from
// b·plane.x, differs between source and other: a group adds its bands once.
[numthreads(16, 16, 1)]
void compare(uint3 id : SV_DispatchThreadID, uint index : SV_GroupIndex) {
	if (index == 0) {
		differing = 0;
	}
	GroupMemoryBarrierWithGroupSync();
	int x = id.x, y = id.y;
	if (x < region.z && y < region.w) {
		int3 at = int3(region.x + x, region.y + y, 0);
		if (any(source.Load(at) != other.Load(at))) {
			InterlockedOr(differing, 1u << min(y / plane.x, 31));
		}
	}
	GroupMemoryBarrierWithGroupSync();
	if (index == 0 && differing != 0) {
		target.InterlockedOr(0, differing);
	}
}
