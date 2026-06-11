-- Dependency edges of symlink/template/case parameters: the referrer breaks if
-- any of its targets (every symlink/case hop on its resolution path plus the
-- terminal node) is deleted. Maintained by the admin write paths; the
-- deletion-time referential check is a single lookup by TargetID. The table is
-- backfilled by the admin server on startup when it is empty.
CREATE TABLE `my_config_tree_dep` (
    `ReferrerID` bigint(20) unsigned NOT NULL,
    `TargetID` bigint(20) unsigned NOT NULL,
    PRIMARY KEY  (`ReferrerID`,`TargetID`),
    KEY `TargetID` (`TargetID`),
    CONSTRAINT `my_config_tree_dep_ibfk_1` FOREIGN KEY (`ReferrerID`) REFERENCES `my_config_tree` (`ID`) ON DELETE CASCADE,
    CONSTRAINT `my_config_tree_dep_ibfk_2` FOREIGN KEY (`TargetID`) REFERENCES `my_config_tree` (`ID`) ON DELETE CASCADE
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

-- Records the symlink depth my_config_tree_dep was last fully built at, so the
-- admin server rebuilds the edges when the configured depth is raised.
CREATE TABLE `my_config_tree_dep_meta` (
    `Depth` int(11) NOT NULL
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

-- Used by the referrer scans (dependency backfill and heal-on-create).
ALTER TABLE `my_config_tree`
    ADD KEY `ContentType` (`ContentType`);
