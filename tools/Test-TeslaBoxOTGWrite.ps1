[CmdletBinding()]
param(
    [Parameter(Mandatory = $true)]
    [ValidatePattern('^[A-Za-z]:\\$')]
    [string]$DriveRoot,

    [ValidateRange(1, 1440)]
    [int]$DurationMinutes = 15,

    [ValidateRange(10, 300)]
    [int]$SegmentSeconds = 60,

    [string]$OutputDirectory = (Join-Path $env:TEMP 'teslabox-otg-write'),

    [switch]$KeepFiles,

    [switch]$SkipVolumeIdentityCheck
)

$ErrorActionPreference = 'Stop'
Set-StrictMode -Version 2.0

$CameraRatesMbps = [ordered]@{
    front          = 10.5
    back           = 5.3
    left_repeater  = 5.3
    right_repeater = 5.3
    left_pillar    = 5.3
    right_pillar   = 5.3
}

$TickMilliseconds = 250
$ChunkBytes = 1MB
$SafetyReserveBytes = 2GB
$TestDirectoryPrefix = 'TeslaBox-OTG-Write-Test-'
$Streams = @{}
$CameraWrittenBytes = @{}
$Metrics = New-Object System.Collections.Generic.List[object]
$FlushSamples = New-Object System.Collections.Generic.List[object]
$TestDirectory = $null
$TestStartedAt = $null
$TestCompletedAt = $null
$Failure = $null
$TotalWrittenBytes = [int64]0
$MaxWriteMilliseconds = 0.0
$SlowWriteCount = 0
$ZeroThroughputWindows = 0

function Format-ByteSize {
    param([int64]$Bytes)

    if ($Bytes -ge 1GB) {
        return ('{0:N2} GiB' -f ($Bytes / 1GB))
    }
    if ($Bytes -ge 1MB) {
        return ('{0:N2} MiB' -f ($Bytes / 1MB))
    }
    return ('{0:N0} bytes' -f $Bytes)
}

function Close-TestStreams {
    foreach ($Stream in @($Streams.Values)) {
        if ($null -ne $Stream) {
            try {
                $Stream.Dispose()
            }
            catch {
                Write-Warning "Failed to close a test file: $($_.Exception.Message)"
            }
        }
    }
    $Streams.Clear()
}

function Open-SegmentStreams {
    param([int]$SegmentIndex)

    foreach ($Camera in $CameraRatesMbps.Keys) {
        $FileName = 'segment-{0:D4}-{1}.bin' -f $SegmentIndex, $Camera
        $FilePath = Join-Path $TestDirectory $FileName
        $Streams[$Camera] = New-Object System.IO.FileStream(
            $FilePath,
            [System.IO.FileMode]::CreateNew,
            [System.IO.FileAccess]::Write,
            [System.IO.FileShare]::Read,
            1MB,
            [System.IO.FileOptions]::SequentialScan
        )
    }
}

function Flush-And-RotateSegment {
    param(
        [int]$CompletedSegmentIndex,
        [int]$NextSegmentIndex,
        [switch]$OpenNext
    )

    $FlushTotal = [System.Diagnostics.Stopwatch]::StartNew()
    $FlushMaxMilliseconds = 0.0
    foreach ($Camera in $CameraRatesMbps.Keys) {
        $Stream = $Streams[$Camera]
        $FlushTimer = [System.Diagnostics.Stopwatch]::StartNew()
        $Stream.Flush($true)
        $FlushTimer.Stop()
        if ($FlushTimer.Elapsed.TotalMilliseconds -gt $FlushMaxMilliseconds) {
            $FlushMaxMilliseconds = $FlushTimer.Elapsed.TotalMilliseconds
        }
        $Stream.Dispose()
    }
    $Streams.Clear()
    $FlushTotal.Stop()

    $FlushSamples.Add([pscustomobject]@{
        segment_index = $CompletedSegmentIndex
        completed_at = (Get-Date).ToString('o')
        total_ms = [math]::Round($FlushTotal.Elapsed.TotalMilliseconds, 2)
        max_file_ms = [math]::Round($FlushMaxMilliseconds, 2)
    })

    if ($OpenNext) {
        Open-SegmentStreams -SegmentIndex $NextSegmentIndex
    }
}

function Remove-TestDirectorySafely {
    if ([string]::IsNullOrWhiteSpace($TestDirectory) -or -not (Test-Path -LiteralPath $TestDirectory -PathType Container)) {
        return
    }

    $CanonicalRoot = [System.IO.Path]::GetFullPath($DriveRoot)
    $CanonicalTarget = [System.IO.Path]::GetFullPath($TestDirectory)
    $TargetInfo = Get-Item -LiteralPath $CanonicalTarget
    $ParentPath = [System.IO.Path]::GetFullPath($TargetInfo.Parent.FullName + [System.IO.Path]::DirectorySeparatorChar)

    if ($ParentPath -ne $CanonicalRoot -or -not $TargetInfo.Name.StartsWith($TestDirectoryPrefix, [System.StringComparison]::Ordinal)) {
        throw "Refusing to remove a directory that failed the boundary check: $CanonicalTarget"
    }

    Remove-Item -LiteralPath $CanonicalTarget -Recurse -Force
}

$DriveRoot = $DriveRoot.ToUpperInvariant()
$DriveLetter = $DriveRoot.Substring(0, 1)
$Drive = New-Object System.IO.DriveInfo($DriveLetter)
if (-not $Drive.IsReady) {
    throw "Target drive $DriveRoot is not ready."
}

if (-not $SkipVolumeIdentityCheck) {
    if ($Drive.VolumeLabel -ne 'TESLACAM') {
        throw "Target volume label is '$($Drive.VolumeLabel)', not TESLACAM. The test was stopped to avoid writing to the wrong disk."
    }
    if ($Drive.DriveFormat -ne 'exFAT') {
        throw "Target filesystem is '$($Drive.DriveFormat)', not the expected exFAT. The test was stopped."
    }
}

$TotalRateMbps = ($CameraRatesMbps.Values | Measure-Object -Sum).Sum
$DurationSeconds = $DurationMinutes * 60
$EstimatedBytes = [int64][math]::Ceiling(($TotalRateMbps * 1000000 / 8) * $DurationSeconds)
$RequiredBytes = $EstimatedBytes + $SafetyReserveBytes
if ($Drive.AvailableFreeSpace -lt $RequiredBytes) {
    throw "TESLACAM free space is insufficient: $(Format-ByteSize $Drive.AvailableFreeSpace) available, $(Format-ByteSize $RequiredBytes) required."
}

$OutputDirectory = [System.IO.Path]::GetFullPath($OutputDirectory)
New-Item -ItemType Directory -Path $OutputDirectory -Force | Out-Null
$RunID = Get-Date -Format 'yyyyMMdd-HHmmss'
$TestDirectory = Join-Path $DriveRoot ($TestDirectoryPrefix + $RunID)
New-Item -ItemType Directory -Path $TestDirectory | Out-Null

$RandomBuffer = New-Object byte[] $ChunkBytes
$RandomGenerator = New-Object System.Security.Cryptography.RNGCryptoServiceProvider
try {
    $RandomGenerator.GetBytes($RandomBuffer)
}
finally {
    $RandomGenerator.Dispose()
}

foreach ($Camera in $CameraRatesMbps.Keys) {
    $CameraWrittenBytes[$Camera] = [int64]0
}

$SummaryPath = Join-Path $OutputDirectory ("otg-write-$RunID.json")
$MetricsPath = Join-Path $OutputDirectory ("otg-write-$RunID.csv")
$FlushPath = Join-Path $OutputDirectory ("otg-write-$RunID-flush.csv")

Write-Host 'TeslaBox OTG six-camera sustained write test'
Write-Host "Target: $DriveRoot  Label: $($Drive.VolumeLabel)  Filesystem: $($Drive.DriveFormat)"
Write-Host "Load: $TotalRateMbps Mbps  Duration: $DurationMinutes minutes  Estimated data: $(Format-ByteSize $EstimatedBytes)"
Write-Host "Test directory: $TestDirectory"
Write-Host 'Do not disconnect OTG, reboot the board, or open test files during the test.'

$SegmentIndex = 1
$CurrentSegmentStartedAtSeconds = 0.0
$LastMetricAtSeconds = 0.0
$LastMetricBytes = [int64]0
$WindowMaxWriteMilliseconds = 0.0
$Clock = [System.Diagnostics.Stopwatch]::StartNew()
$TestStartedAt = Get-Date

try {
    Open-SegmentStreams -SegmentIndex $SegmentIndex

    while ($Clock.Elapsed.TotalSeconds -lt $DurationSeconds) {
        $ElapsedSeconds = $Clock.Elapsed.TotalSeconds

        if (($ElapsedSeconds - $CurrentSegmentStartedAtSeconds) -ge $SegmentSeconds) {
            Flush-And-RotateSegment -CompletedSegmentIndex $SegmentIndex -NextSegmentIndex ($SegmentIndex + 1) -OpenNext
            $SegmentIndex++
            $CurrentSegmentStartedAtSeconds = $ElapsedSeconds
        }

        foreach ($Camera in $CameraRatesMbps.Keys) {
            $BytesPerSecond = [double]$CameraRatesMbps[$Camera] * 1000000 / 8
            $TargetBytes = [int64][math]::Floor($BytesPerSecond * $ElapsedSeconds)
            $BytesDue = $TargetBytes - [int64]$CameraWrittenBytes[$Camera]

            while ($BytesDue -gt 0) {
                $WriteBytes = [int][math]::Min($ChunkBytes, $BytesDue)
                $WriteTimer = [System.Diagnostics.Stopwatch]::StartNew()
                $Streams[$Camera].Write($RandomBuffer, 0, $WriteBytes)
                $WriteTimer.Stop()

                $WriteMilliseconds = $WriteTimer.Elapsed.TotalMilliseconds
                if ($WriteMilliseconds -gt $MaxWriteMilliseconds) {
                    $MaxWriteMilliseconds = $WriteMilliseconds
                }
                if ($WriteMilliseconds -gt $WindowMaxWriteMilliseconds) {
                    $WindowMaxWriteMilliseconds = $WriteMilliseconds
                }
                if ($WriteMilliseconds -ge 1000) {
                    $SlowWriteCount++
                }

                $CameraWrittenBytes[$Camera] = [int64]$CameraWrittenBytes[$Camera] + $WriteBytes
                $TotalWrittenBytes += $WriteBytes
                $BytesDue -= $WriteBytes
            }
        }

        $NowSeconds = $Clock.Elapsed.TotalSeconds
        if (($NowSeconds - $LastMetricAtSeconds) -ge 1.0) {
            $WindowSeconds = $NowSeconds - $LastMetricAtSeconds
            $WindowBytes = $TotalWrittenBytes - $LastMetricBytes
            $ThroughputMbps = if ($WindowSeconds -gt 0) { ($WindowBytes * 8 / 1000000) / $WindowSeconds } else { 0 }
            if ($WindowBytes -eq 0) {
                $ZeroThroughputWindows++
            }

            $ScheduledBytes = [int64][math]::Floor(($TotalRateMbps * 1000000 / 8) * $NowSeconds)
            $LagMilliseconds = [math]::Max(0, (($ScheduledBytes - $TotalWrittenBytes) * 8 / ($TotalRateMbps * 1000000)) * 1000)
            $Metrics.Add([pscustomobject]@{
                elapsed_seconds = [math]::Round($NowSeconds, 3)
                throughput_mbps = [math]::Round($ThroughputMbps, 3)
                schedule_lag_ms = [math]::Round($LagMilliseconds, 2)
                max_write_ms = [math]::Round($WindowMaxWriteMilliseconds, 2)
                total_bytes = $TotalWrittenBytes
            })

            if (([int]$NowSeconds % 10) -eq 0) {
                Write-Host ('[{0,5:N0}s] {1,6:N2} Mbps  written {2}  lag {3:N0} ms  slowest write {4:N0} ms' -f $NowSeconds, $ThroughputMbps, (Format-ByteSize $TotalWrittenBytes), $LagMilliseconds, $WindowMaxWriteMilliseconds)
            }

            $LastMetricAtSeconds = $NowSeconds
            $LastMetricBytes = $TotalWrittenBytes
            $WindowMaxWriteMilliseconds = 0.0
        }

        Start-Sleep -Milliseconds $TickMilliseconds
    }

    Flush-And-RotateSegment -CompletedSegmentIndex $SegmentIndex -NextSegmentIndex 0
}
catch {
    $Failure = $_.Exception.Message
    throw
}
finally {
    $Clock.Stop()
    $TestCompletedAt = Get-Date
    Close-TestStreams

    $Metrics | Export-Csv -LiteralPath $MetricsPath -NoTypeInformation -Encoding UTF8
    $FlushSamples | Export-Csv -LiteralPath $FlushPath -NoTypeInformation -Encoding UTF8

    $AverageMbps = if ($Clock.Elapsed.TotalSeconds -gt 0) { ($TotalWrittenBytes * 8 / 1000000) / $Clock.Elapsed.TotalSeconds } else { 0 }
    $MinimumWindowMbps = if ($Metrics.Count -gt 0) { ($Metrics | Measure-Object -Property throughput_mbps -Minimum).Minimum } else { 0 }
    $MaximumLagMilliseconds = if ($Metrics.Count -gt 0) { ($Metrics | Measure-Object -Property schedule_lag_ms -Maximum).Maximum } else { 0 }
    $MaximumFlushMilliseconds = if ($FlushSamples.Count -gt 0) { ($FlushSamples | Measure-Object -Property total_ms -Maximum).Maximum } else { 0 }

    $Summary = [ordered]@{
        run_id = $RunID
        started_at = if ($null -ne $TestStartedAt) { $TestStartedAt.ToString('o') } else { $null }
        completed_at = $TestCompletedAt.ToString('o')
        success = [string]::IsNullOrEmpty($Failure)
        error = $Failure
        drive_root = $DriveRoot
        volume_label = $Drive.VolumeLabel
        filesystem = $Drive.DriveFormat
        duration_seconds = [math]::Round($Clock.Elapsed.TotalSeconds, 3)
        configured_total_mbps = $TotalRateMbps
        written_bytes = $TotalWrittenBytes
        average_mbps = [math]::Round($AverageMbps, 3)
        minimum_one_second_mbps = [math]::Round([double]$MinimumWindowMbps, 3)
        maximum_schedule_lag_ms = [math]::Round([double]$MaximumLagMilliseconds, 2)
        maximum_write_ms = [math]::Round($MaxWriteMilliseconds, 2)
        writes_over_one_second = $SlowWriteCount
        zero_throughput_windows = $ZeroThroughputWindows
        maximum_segment_flush_ms = [math]::Round([double]$MaximumFlushMilliseconds, 2)
        test_files_kept = [bool]$KeepFiles
        metrics_csv = $MetricsPath
        flush_csv = $FlushPath
    }
    $Summary | ConvertTo-Json -Depth 4 | Set-Content -LiteralPath $SummaryPath -Encoding UTF8

    if (-not $KeepFiles) {
        try {
            Remove-TestDirectorySafely
        }
        catch {
            Write-Warning "Test files could not be safely removed: $($_.Exception.Message)"
        }
    }

    Write-Host "Test report: $SummaryPath"
    Write-Host "Average: $([math]::Round($AverageMbps, 2)) Mbps; slowest write: $([math]::Round($MaxWriteMilliseconds, 0)) ms; longest segment flush: $([math]::Round([double]$MaximumFlushMilliseconds, 0)) ms"
    if (-not $KeepFiles) {
        Write-Host 'Test files were removed from TESLACAM. Reclaim the outer sparse-image space through TeslaBox only after OTG is safely disconnected.'
    }
}
