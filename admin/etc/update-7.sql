ALTER TABLE `my_config_tree_log`
    ADD COLUMN `Path` varchar(512) character set ascii collate ascii_bin NOT NULL default '' AFTER `NodeID`,
    ADD KEY `Path` (`Path`);

-- Backfill the historical path with each node's current path. Versions written
-- before this migration thus get the node's present location rather than the
-- one they were actually written at; that imprecision is unavoidable and only
-- affects nodes that were moved before the upgrade.
UPDATE `my_config_tree_log` l
    JOIN `my_config_tree` t ON t.`ID` = l.`NodeID`
    SET l.`Path` = t.`Path`
    WHERE l.`Path` = '';
